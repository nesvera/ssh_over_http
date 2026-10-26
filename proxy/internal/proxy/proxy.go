// Package proxy pairs client WebSocket connections with streams multiplexed
// over each device agent's single WebSocket.
package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"sshoverhttp/proxy/internal/mux"
)

const (
	pingInterval   = 20 * time.Second
	pingTimeout    = 10 * time.Second
	openTimeout    = 10 * time.Second
	clientMaxFrame = 1 << 20
)

type Config struct {
	// AgentTokens maps device ID to the bearer token its agent must present.
	AgentTokens map[string]string
	// ClientTokens are bearer tokens accepted from clients. Any valid client
	// token may reach any device for now.
	ClientTokens []string
}

type Server struct {
	cfg Config

	mu     sync.Mutex
	agents map[string]*mux.Session
}

func New(cfg Config) *Server {
	return &Server{cfg: cfg, agents: make(map[string]*mux.Session)}
}

func (p *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /agent", p.handleAgent)
	m.HandleFunc("GET /connect", p.handleConnect)
	m.HandleFunc("GET /devices", p.handleDevices)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	return m
}

func (p *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	want, ok := p.cfg.AgentTokens[device]
	if device == "" || !ok || !tokenEqual(bearer(r), want) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		log.Printf("agent %s: accept: %v", device, err)
		return
	}
	sess := mux.NewSession(r.Context(), c)
	p.register(device, sess)
	log.Printf("agent %s: connected from %s", device, r.RemoteAddr)

	go keepalive(sess)
	err = sess.Run()
	p.unregister(device, sess)
	log.Printf("agent %s: disconnected: %v", device, err)
}

func (p *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !p.clientAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	device := r.URL.Query().Get("device")
	sess := p.lookup(device)
	if sess == nil {
		http.Error(w, "device not connected", http.StatusNotFound)
		return
	}

	// Open the stream before upgrading so failures surface as HTTP errors.
	ctx, cancel := context.WithTimeout(r.Context(), openTimeout)
	st, err := sess.Open(ctx)
	cancel()
	if err != nil {
		log.Printf("connect %s: open stream: %v", device, err)
		http.Error(w, "could not reach device ssh server", http.StatusBadGateway)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		st.Close()
		log.Printf("connect %s: accept: %v", device, err)
		return
	}
	c.SetReadLimit(clientMaxFrame)
	log.Printf("connect %s: session opened for %s", device, r.RemoteAddr)

	pipe(websocket.NetConn(r.Context(), c, websocket.MessageBinary), st)
	log.Printf("connect %s: session closed for %s", device, r.RemoteAddr)
}

func (p *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	if !p.clientAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	p.mu.Lock()
	devices := make([]string, 0, len(p.agents))
	for id := range p.agents {
		devices = append(devices, id)
	}
	p.mu.Unlock()
	sort.Strings(devices)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(devices)
}

func (p *Server) register(device string, sess *mux.Session) {
	p.mu.Lock()
	old := p.agents[device]
	p.agents[device] = sess
	p.mu.Unlock()
	if old != nil {
		log.Printf("agent %s: replacing previous connection", device)
		old.Close()
	}
}

func (p *Server) unregister(device string, sess *mux.Session) {
	p.mu.Lock()
	if p.agents[device] == sess {
		delete(p.agents, device)
	}
	p.mu.Unlock()
}

func (p *Server) lookup(device string) *mux.Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.agents[device]
}

func (p *Server) clientAuthorized(r *http.Request) bool {
	got := bearer(r)
	for _, t := range p.cfg.ClientTokens {
		if tokenEqual(got, t) {
			return true
		}
	}
	return false
}

func keepalive(sess *mux.Session) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-sess.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
			err := sess.Ping(ctx)
			cancel()
			if err != nil {
				sess.Close()
				return
			}
		}
	}
}

// pipe copies in both directions until either side finishes, then closes both.
func pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
	a.Close()
	b.Close()
	<-done
}

func bearer(r *http.Request) string {
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return token
}

func tokenEqual(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
