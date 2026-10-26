// Package mux implements the proxy side of the stream multiplexing protocol
// spoken over a single WebSocket between the proxy and a device agent.
//
// Every binary WebSocket message carries exactly one frame:
//
//	+--------+-------------------+-------------------+
//	| type:1 | stream id:4 (BE)  | payload:0..32KiB  |
//	+--------+-------------------+-------------------+
//
// The proxy opens streams (OPEN); the agent answers OPEN_OK or OPEN_FAIL.
// DATA is flow controlled per stream and per direction: OPEN and OPEN_OK each
// advertise the sender's receive window, the peer may have at most that many
// unacknowledged bytes in flight, and the receiver returns credit with WINDOW
// frames as it consumes data. Advertising (rather than assuming a shared
// constant) lets proxies and agents of different versions interoperate.
// CLOSE tears a stream down in both directions.
//
// The agent implementation lives in target/agent/src/mux.js and must stay in sync.
package mux

import "encoding/binary"

const (
	FrameOpen     byte = 0x01 // proxy -> agent, payload: uint32 BE receive window
	FrameOpenOK   byte = 0x02 // agent -> proxy, payload: uint32 BE receive window
	FrameOpenFail byte = 0x03 // agent -> proxy, payload: UTF-8 reason
	FrameData     byte = 0x04 // both ways, payload: stream bytes
	FrameClose    byte = 0x05 // both ways, empty payload
	FrameWindow   byte = 0x06 // both ways, payload: uint32 BE credit increment
)

const (
	HeaderSize = 5
	MaxPayload = 32 * 1024
	// InitialWindow is the receive window we advertise. It bounds per-stream
	// throughput to InitialWindow/RTT, and matches the 2 MiB SSH channel window
	// (OpenSSH and x/crypto/ssh) so the tunnel never caps a transfer below what
	// SSH itself allows.
	InitialWindow = 2 * 1024 * 1024
)

func encodeFrame(typ byte, id uint32, payload []byte) []byte {
	buf := make([]byte, HeaderSize+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], id)
	copy(buf[HeaderSize:], payload)
	return buf
}

func uint32Bytes(n int) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(n))
	return b
}
