package mux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/coder/websocket"
)

var (
	ErrSessionClosed = errors.New("mux: session closed")
	ErrStreamClosed  = errors.New("mux: stream closed")
)

// Session multiplexes streams over one WebSocket connected to an agent.
type Session struct {
	ctx  context.Context
	conn *websocket.Conn

	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  uint32
	err     error

	done chan struct{}
}

func NewSession(ctx context.Context, conn *websocket.Conn) *Session {
	conn.SetReadLimit(HeaderSize + MaxPayload)
	return &Session{
		ctx:     ctx,
		conn:    conn,
		streams: make(map[uint32]*Stream),
		nextID:  1,
		done:    make(chan struct{}),
	}
}

// Done is closed once the session has terminated.
func (s *Session) Done() <-chan struct{} { return s.done }

// Close terminates the session and every stream on it.
func (s *Session) Close() error {
	s.shutdown(ErrSessionClosed)
	return s.conn.Close(websocket.StatusNormalClosure, "")
}

// Ping round-trips a WebSocket ping. Run must be active.
func (s *Session) Ping(ctx context.Context) error { return s.conn.Ping(ctx) }

// Run reads frames until the connection fails. It must be running for Open,
// Ping and stream I/O to make progress.
func (s *Session) Run() error {
	for {
		typ, msg, err := s.conn.Read(s.ctx)
		if err != nil {
			s.shutdown(err)
			return err
		}
		if typ != websocket.MessageBinary || len(msg) < HeaderSize {
			err := errors.New("mux: malformed frame")
			s.shutdown(err)
			s.conn.Close(websocket.StatusProtocolError, err.Error())
			return err
		}
		id := binary.BigEndian.Uint32(msg[1:5])
		payload := msg[HeaderSize:]

		s.mu.Lock()
		st := s.streams[id]
		s.mu.Unlock()
		if st == nil {
			// Frames racing with a close we already processed.
			continue
		}

		switch msg[0] {
		case FrameOpenOK:
			if len(payload) != 4 {
				s.remove(id)
				st.resolveOpen(errors.New("mux: OPEN_OK without receive window"))
				st.Close()
				continue
			}
			st.addSendWindow(int(binary.BigEndian.Uint32(payload)))
			st.resolveOpen(nil)
		case FrameOpenFail:
			s.remove(id)
			st.resolveOpen(fmt.Errorf("mux: agent refused stream: %s", payload))
		case FrameData:
			if err := st.pushData(payload); err != nil {
				st.Close()
			}
		case FrameWindow:
			if len(payload) == 4 {
				st.addSendWindow(int(binary.BigEndian.Uint32(payload)))
			}
		case FrameClose:
			s.remove(id)
			st.remoteClose()
		}
	}
}

// Open asks the agent for a new stream to its SSH server.
func (s *Session) Open(ctx context.Context) (*Stream, error) {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, s.err
	}
	id := s.nextID
	s.nextID++
	st := newStream(s, id)
	s.streams[id] = st
	s.mu.Unlock()

	if err := s.writeFrame(FrameOpen, id, uint32Bytes(InitialWindow)); err != nil {
		s.remove(id)
		return nil, err
	}

	select {
	case err := <-st.opened:
		if err != nil {
			return nil, err
		}
		return st, nil
	case <-ctx.Done():
		st.Close()
		return nil, ctx.Err()
	case <-s.done:
		return nil, s.err
	}
}

func (s *Session) writeFrame(typ byte, id uint32, payload []byte) error {
	return s.conn.Write(s.ctx, websocket.MessageBinary, encodeFrame(typ, id, payload))
}

func (s *Session) remove(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}

func (s *Session) shutdown(err error) {
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return
	}
	s.err = err
	streams := s.streams
	s.streams = make(map[uint32]*Stream)
	s.mu.Unlock()

	for _, st := range streams {
		st.sessionClosed(err)
	}
	close(s.done)
}

// Stream is one bidirectional byte stream to the agent's SSH server.
type Stream struct {
	id uint32
	s  *Session

	opened chan error

	mu   sync.Mutex
	cond *sync.Cond
	buf  bytes.Buffer

	recvWindow int // credit the agent still has with us
	unacked    int // bytes read by the consumer but not yet credited back
	sendWindow int // credit we still have with the agent; set by OPEN_OK

	closed       bool // closed locally
	remoteClosed bool
	sessErr      error
}

func newStream(s *Session, id uint32) *Stream {
	st := &Stream{
		id:         id,
		s:          s,
		opened:     make(chan error, 1),
		recvWindow: InitialWindow,
	}
	st.cond = sync.NewCond(&st.mu)
	return st
}

func (st *Stream) Read(p []byte) (int, error) {
	st.mu.Lock()
	for st.buf.Len() == 0 && !st.closed && !st.remoteClosed && st.sessErr == nil {
		st.cond.Wait()
	}
	if st.buf.Len() == 0 {
		err := st.terminalErrLocked(io.EOF)
		st.mu.Unlock()
		return 0, err
	}
	n, _ := st.buf.Read(p)
	st.unacked += n
	// Batch credit updates, but never sit on credit once the buffer is empty
	// or the sender could stall waiting for it.
	credit := 0
	if st.unacked >= InitialWindow/4 || st.buf.Len() == 0 {
		credit, st.unacked = st.unacked, 0
		st.recvWindow += credit
	}
	st.mu.Unlock()

	if credit > 0 {
		st.s.writeFrame(FrameWindow, st.id, uint32Bytes(credit))
	}
	return n, nil
}

func (st *Stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		st.mu.Lock()
		for st.sendWindow == 0 && !st.closed && !st.remoteClosed && st.sessErr == nil {
			st.cond.Wait()
		}
		if st.closed || st.remoteClosed || st.sessErr != nil {
			err := st.terminalErrLocked(ErrStreamClosed)
			st.mu.Unlock()
			return written, err
		}
		n := min(len(p), st.sendWindow, MaxPayload)
		st.sendWindow -= n
		st.mu.Unlock()

		if err := st.s.writeFrame(FrameData, st.id, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// Close closes the stream in both directions and tells the agent.
func (st *Stream) Close() error {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return nil
	}
	st.closed = true
	notify := !st.remoteClosed && st.sessErr == nil
	st.cond.Broadcast()
	st.mu.Unlock()

	st.s.remove(st.id)
	if notify {
		return st.s.writeFrame(FrameClose, st.id, nil)
	}
	return nil
}

// terminalErrLocked picks the error to report once no more I/O is possible.
func (st *Stream) terminalErrLocked(remoteErr error) error {
	switch {
	case st.closed:
		return ErrStreamClosed
	case st.remoteClosed:
		return remoteErr
	default:
		return st.sessErr
	}
}

func (st *Stream) resolveOpen(err error) {
	select {
	case st.opened <- err:
	default:
	}
}

func (st *Stream) pushData(p []byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(p) > st.recvWindow {
		return errors.New("mux: agent exceeded flow control window")
	}
	st.recvWindow -= len(p)
	st.buf.Write(p)
	st.cond.Broadcast()
	return nil
}

func (st *Stream) addSendWindow(n int) {
	st.mu.Lock()
	st.sendWindow += n
	st.cond.Broadcast()
	st.mu.Unlock()
}

func (st *Stream) remoteClose() {
	st.mu.Lock()
	st.remoteClosed = true
	st.cond.Broadcast()
	st.mu.Unlock()
	st.resolveOpen(ErrStreamClosed)
}

func (st *Stream) sessionClosed(err error) {
	st.mu.Lock()
	st.sessErr = err
	st.cond.Broadcast()
	st.mu.Unlock()
	st.resolveOpen(err)
}
