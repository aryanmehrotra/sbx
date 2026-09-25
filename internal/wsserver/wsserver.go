// Package wsserver is the server half of RFC 6455, standard library only.
//
// Why a third WebSocket implementation: internal/daemon has a server subset, but it relays
// opaque bytes and so throws away whether a message was text or binary, and execd's PTY protocol
// is built on exactly that distinction (JSON control frames are text, terminal bytes are binary).
// execd also must not import the daemon: it ships inside every sandbox and the daemon does not.
// internal/wsclient is the client half and is what the tests here drive this with.
//
// What it does not do: compression, extensions or subprotocols. A client that asks for any of
// them is answered without them, which RFC 6455 allows and every browser and SDK accepts.
//
// The parser is reachable by anything holding the execd token, so it is attack surface: lengths
// are checked before allocation, unmasked client frames and reserved bits fail the connection.
package wsserver

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// DefaultMaxMessage caps a reassembled client message. Terminal input arrives a keystroke or a
// paste at a time; the cap exists because a client declares a length before sending it, and one
// header could otherwise ask for an allocation of terabytes.
const DefaultMaxMessage = 4 << 20

// MessageType is the opcode of a data message.
type MessageType int

// Data message types.
const (
	TextMessage   MessageType = 0x1
	BinaryMessage MessageType = 0x2
)

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// Close status codes.
const (
	CloseNormal        = 1000
	CloseGoingAway     = 1001
	CloseProtocolError = 1002
	CloseInvalidData   = 1007
	ClosePolicy        = 1008
	CloseTooBig        = 1009
	closeNoStatus      = 1005
)

// CloseError is returned by ReadMessage once the client has closed the connection.
type CloseError struct {
	Code int
	Text string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("websocket closed by client (code %d %s)", e.Code, e.Text)
}

// ErrClosed is returned by writes after the connection was closed.
var ErrClosed = errors.New("websocket: connection already closed")

// IsUpgrade reports whether r asks for a WebSocket upgrade.
func IsUpgrade(r *http.Request) bool {
	return headerHas(r.Header, "Connection", "upgrade") && strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

func headerHas(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}

	return false
}

// Options configures a connection.
type Options struct {
	// WriteTimeout bounds each write. Zero means no bound - which leaves a writer blocked forever
	// behind a client that stopped reading, so callers streaming output should set it.
	WriteTimeout time.Duration
	// MaxMessage caps a reassembled message; zero means DefaultMaxMessage.
	MaxMessage int64
}

// Conn is one upgraded connection. Reads must come from a single goroutine; writes and Close
// are safe to call concurrently with each other and with the reader.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	opt  Options

	wmu    sync.Mutex
	closed bool // a close frame was sent or the socket closed; guarded by wmu

	pongMu   sync.Mutex
	lastPong time.Time
}

// Upgrade completes the handshake and takes the connection over. On a request that is not a
// valid upgrade it answers 400 itself, as gorilla/websocket does, so the caller only has to
// return.
func Upgrade(w http.ResponseWriter, r *http.Request, opt Options) (*Conn, error) {
	fail := func(msg string) (*Conn, error) {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "websocket: "+msg, http.StatusBadRequest)

		return nil, errors.New("websocket: " + msg)
	}

	if r.Method != http.MethodGet {
		return fail("the upgrade request must be a GET")
	}

	if !IsUpgrade(r) {
		return fail("not a websocket upgrade: send Connection: Upgrade and Upgrade: websocket")
	}

	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return fail("unsupported Sec-WebSocket-Version; this server speaks version 13")
	}

	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return fail("missing Sec-WebSocket-Key")
	}

	// Through ResponseController so wrappers that implement Unwrap (execd's recoverer) still
	// reach the real connection.
	nc, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, fmt.Errorf("websocket: take over the connection: %w", err)
	}

	// The http.Server's deadlines carry over to the hijacked socket, and a terminal outlives any
	// of them.
	_ = nc.SetDeadline(time.Time{})

	_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n\r\n", Accept(key))
	if err == nil {
		err = rw.Flush()
	}

	if err != nil {
		_ = nc.Close()

		return nil, fmt.Errorf("websocket: write handshake: %w", err)
	}

	if opt.MaxMessage <= 0 {
		opt.MaxMessage = DefaultMaxMessage
	}

	return &Conn{conn: nc, br: rw.Reader, opt: opt, lastPong: time.Now()}, nil
}

// Accept computes Sec-WebSocket-Accept for a key.
func Accept(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+magic)

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ReadMessage returns the next complete data message, answering pings and reassembling
// fragments. After the client closes it returns a *CloseError, having echoed the close.
func (c *Conn) ReadMessage() (MessageType, []byte, error) {
	var (
		assembled []byte
		msgType   MessageType
		inMessage bool
	)

	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}

		switch op {
		case opPing:
			if err := c.write(opPong, payload, 0); err != nil && !errors.Is(err, ErrClosed) {
				return 0, nil, err
			}

			continue
		case opPong:
			c.pongMu.Lock()
			c.lastPong = time.Now()
			c.pongMu.Unlock()

			continue
		case opClose:
			return 0, nil, c.onClose(payload)
		case opText, opBinary:
			if inMessage {
				return 0, nil, c.fail(CloseProtocolError, "data frame inside a fragmented message")
			}

			msgType, inMessage = MessageType(op), true
		case opContinuation:
			if !inMessage {
				return 0, nil, c.fail(CloseProtocolError, "continuation frame with no message to continue")
			}
		default:
			return 0, nil, c.fail(CloseProtocolError, fmt.Sprintf("unknown opcode %#x", op))
		}

		if int64(len(assembled))+int64(len(payload)) > c.opt.MaxMessage {
			return 0, nil, c.fail(CloseTooBig, fmt.Sprintf("message over the %d byte limit", c.opt.MaxMessage))
		}

		assembled = append(assembled, payload...)

		if !fin {
			continue
		}

		if msgType == TextMessage && !utf8.Valid(assembled) {
			return 0, nil, c.fail(CloseInvalidData, "text message is not valid UTF-8")
		}

		if assembled == nil {
			assembled = []byte{}
		}

		return msgType, assembled, nil
	}
}

func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}

	fin = head[0]&0x80 != 0
	op = head[0] & 0x0f

	if head[0]&0x70 != 0 {
		return false, 0, nil, c.fail(CloseProtocolError, "reserved bits set with no extension negotiated")
	}

	// A client MUST mask; an unmasked frame is how content gets smuggled past a proxy reading
	// plaintext, and the RFC requires failing the connection.
	if head[1]&0x80 == 0 {
		return false, 0, nil, c.fail(CloseProtocolError, "client frame is not masked")
	}

	length := uint64(head[1] & 0x7f)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}

		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}

		length = binary.BigEndian.Uint64(ext[:])
	}

	if op >= opClose && (!fin || length > 125) {
		return false, 0, nil, c.fail(CloseProtocolError, "control frame fragmented or over 125 bytes")
	}

	if length > uint64(c.opt.MaxMessage) {
		return false, 0, nil, c.fail(CloseTooBig, fmt.Sprintf("frame declares %d bytes, over the %d limit", length, c.opt.MaxMessage))
	}

	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return false, 0, nil, err
	}

	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}

	for i := range payload {
		payload[i] ^= mask[i%4]
	}

	return fin, op, payload, nil
}

func (c *Conn) onClose(payload []byte) error {
	ce := &CloseError{Code: closeNoStatus}
	if len(payload) >= 2 {
		ce.Code = int(binary.BigEndian.Uint16(payload[:2]))
		ce.Text = string(payload[2:])
	}

	echo := payload
	if len(echo) > 2 {
		echo = echo[:2]
	}

	_ = c.write(opClose, echo, time.Second)
	_ = c.conn.Close()

	return ce
}

func (c *Conn) fail(code int, reason string) error {
	_ = c.write(opClose, closePayload(code, reason), time.Second)
	_ = c.conn.Close()

	return fmt.Errorf("websocket: %s", reason)
}

// WriteMessage sends one unfragmented data message.
func (c *Conn) WriteMessage(t MessageType, payload []byte) error {
	if t != TextMessage && t != BinaryMessage {
		return fmt.Errorf("websocket: message type %d is not text or binary", t)
	}

	return c.write(byte(t), payload, c.opt.WriteTimeout)
}

// Ping sends a ping; the pong is recorded by ReadMessage and reported by LastPong.
func (c *Conn) Ping() error { return c.write(opPing, nil, c.opt.WriteTimeout) }

// LastPong is when the client last answered a ping (or when the connection opened).
func (c *Conn) LastPong() time.Time {
	c.pongMu.Lock()
	defer c.pongMu.Unlock()

	return c.lastPong
}

// CloseWith sends a close frame with a status and reason, then closes the socket.
func (c *Conn) CloseWith(code int, reason string) error {
	err := c.write(opClose, closePayload(code, reason), c.opt.WriteTimeout)
	_ = c.conn.Close()

	return err
}

// CloseNow is CloseWith for a caller that must not wait behind a writer stalled on a client
// that stopped reading: the close frame is sent only if the write lock is free, within a short
// bound, and the socket is closed regardless - which is what unblocks the stalled writer.
func (c *Conn) CloseNow(code int, reason string) {
	if c.wmu.TryLock() {
		if !c.closed {
			_ = c.conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
			_, _ = c.conn.Write(frame(opClose, closePayload(code, reason)))
			c.closed = true
		}
		c.wmu.Unlock()
	}

	_ = c.conn.Close()
}

// Close closes the socket without a close frame.
func (c *Conn) Close() error { return c.conn.Close() }

// SetReadDeadline bounds the next ReadMessage.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }

func (c *Conn) write(op byte, payload []byte, timeout time.Duration) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	if c.closed {
		return ErrClosed
	}

	if op == opClose {
		c.closed = true
	}

	if timeout > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(timeout))
	} else {
		_ = c.conn.SetWriteDeadline(time.Time{})
	}

	_, err := c.conn.Write(frame(op, payload))

	return err
}

// frame builds one unmasked, final frame. A server never masks.
func frame(op byte, payload []byte) []byte {
	head := make([]byte, 0, 10+len(payload))
	head = append(head, 0x80|op)

	switch n := len(payload); {
	case n < 126:
		head = append(head, byte(n))
	case n < 1<<16:
		head = append(head, 126, byte(n>>8), byte(n))
	default:
		head = append(head, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(head[2:], uint64(n))
	}

	return append(head, payload...)
}

func closePayload(code int, reason string) []byte {
	if len(reason) > 123 {
		reason = reason[:123]
	}

	p := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(p, uint16(code))

	return append(p, reason...)
}
