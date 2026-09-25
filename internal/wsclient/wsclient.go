// Package wsclient is the client half of RFC 6455, standard library only.
//
// Why a second WebSocket implementation when internal/daemon already has one: that one is a
// server subset, unexported inside package daemon, and shaped around relaying binary TCP chunks.
// This package exists for talking *to* a server that speaks JSON over text frames - a Jupyter
// kernel's channels socket - where fragmentation, UTF-8 validation and the close handshake all
// matter, and where importing the daemon package to reach one unexported type would drag the
// whole wake/sleep state machine into every caller. If the daemon ever needs a full client, this
// is the one to move to.
//
// What it does not do: compression (permessage-deflate), extensions, or subprotocol negotiation.
// A server that insists on any of them refuses the handshake, which Dial reports.
package wsclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// magic is the constant RFC 6455 appends to the key before hashing; it proves the peer understood
// the upgrade rather than a cache replaying a 101 it did not.
const magic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// DefaultMaxMessage caps a reassembled message. A Jupyter result carrying a base64 PNG runs to a
// few MiB; the cap exists because a server declares a length before sending it, and without one a
// single header can ask for an allocation of terabytes.
const DefaultMaxMessage = 64 << 20

// MessageType is the opcode of a data message.
type MessageType int

// Data opcodes a caller sees. Control frames are answered inside ReadMessage.
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

// Close status codes this package sends.
const (
	CloseNormal        = 1000
	CloseProtocolError = 1002
	CloseInvalidData   = 1007
	CloseTooBig        = 1009
	closeNoStatus      = 1005
)

// CloseError is returned by ReadMessage once the server has closed the connection.
type CloseError struct {
	Code int
	Text string
}

func (e *CloseError) Error() string {
	if e.Text == "" {
		return fmt.Sprintf("websocket closed by server (code %d)", e.Code)
	}

	return fmt.Sprintf("websocket closed by server (code %d: %s)", e.Code, e.Text)
}

// ErrClosed is returned by writes after Close.
var ErrClosed = errors.New("websocket: connection already closed")

// Options configures Dial.
type Options struct {
	// Header is sent with the upgrade request - where a bearer or Jupyter token goes.
	Header http.Header
	// TLSConfig is used for wss://. Nil means the system roots and the URL's host name.
	TLSConfig *tls.Config
	// MaxMessage caps a reassembled message; zero means DefaultMaxMessage.
	MaxMessage int64
}

// Conn is one client connection. Reads must come from a single goroutine; writes, Ping and Close
// are safe to call concurrently with each other and with the reader.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	max  int64

	wmu    sync.Mutex
	closed bool // a close frame has been sent; guarded by wmu
}

// Dial opens a WebSocket connection to a ws:// or wss:// URL.
//
// The context bounds the TCP connect and the handshake only; once Dial returns, the connection
// lives until Close or until the server goes away.
func Dial(ctx context.Context, rawURL string, opt Options) (*Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("websocket: parse %q: %w", rawURL, err)
	}

	var secure bool

	switch u.Scheme {
	case "ws":
	case "wss":
		secure = true
	default:
		return nil, fmt.Errorf("websocket: scheme %q is not ws or wss - convert http(s):// before dialing", u.Scheme)
	}

	hostPort := u.Host
	if u.Port() == "" {
		port := "80"
		if secure {
			port = "443"
		}

		hostPort = net.JoinHostPort(u.Hostname(), port)
	}

	var d net.Dialer

	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return nil, fmt.Errorf("websocket: connect %s: %w", hostPort, err)
	}

	// A handshake that hangs would otherwise hang Dial forever: the deadline covers a server that
	// accepts TCP and never answers, and AfterFunc covers a cancel with no deadline at all.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })

	c, err := handshake(conn, u, secure, opt)

	if !stop() || ctx.Err() != nil {
		if c != nil {
			_ = c.conn.Close()
		}

		if err == nil {
			err = ctx.Err()
		}

		return nil, fmt.Errorf("websocket: handshake with %s: %w", hostPort, err)
	}

	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	_ = c.conn.SetDeadline(time.Time{})

	return c, nil
}

func handshake(conn net.Conn, u *url.URL, secure bool, opt Options) (*Conn, error) {
	if secure {
		cfg := opt.TLSConfig
		if cfg == nil {
			cfg = &tls.Config{MinVersion: tls.VersionTLS12}
		} else {
			cfg = cfg.Clone()
		}

		if cfg.ServerName == "" {
			cfg.ServerName = u.Hostname()
		}

		tc := tls.Client(conn, cfg)
		if err := tc.Handshake(); err != nil {
			return nil, fmt.Errorf("websocket: tls: %w", err)
		}

		conn = tc
	}

	key := newKey()

	var b strings.Builder

	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\n", u.RequestURI(), u.Host)
	b.WriteString("Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\n", key)

	for name, values := range opt.Header {
		// The handshake headers are ours; a caller overriding one would produce a request the
		// server answers in a way we then refuse, which is harder to diagnose than a skip.
		switch http.CanonicalHeaderKey(name) {
		case "Host", "Upgrade", "Connection", "Sec-Websocket-Key", "Sec-Websocket-Version":
			continue
		}

		for _, v := range values {
			fmt.Fprintf(&b, "%s: %s\r\n", name, v)
		}
	}

	b.WriteString("\r\n")

	if _, err := io.WriteString(conn, b.String()); err != nil {
		return nil, fmt.Errorf("websocket: send upgrade: %w", err)
	}

	br := bufio.NewReader(conn)

	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, fmt.Errorf("websocket: read upgrade response: %w", err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()

		return nil, &HandshakeError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}

	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(resp.Header.Get("Connection")), "upgrade") {
		return nil, errors.New("websocket: server answered 101 without Upgrade: websocket - something in the path is not a websocket server")
	}

	if resp.Header.Get("Sec-WebSocket-Accept") != accept(key) {
		return nil, errors.New("websocket: Sec-WebSocket-Accept does not match the key - the 101 came from something other than the server")
	}

	if ext := resp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		return nil, fmt.Errorf("websocket: server negotiated extension %q that was never offered", ext)
	}

	maxMsg := opt.MaxMessage
	if maxMsg <= 0 {
		maxMsg = DefaultMaxMessage
	}

	return &Conn{conn: conn, br: br, max: maxMsg}, nil
}

// HandshakeError is a refused upgrade: the server answered with something other than 101.
type HandshakeError struct {
	Status int
	Body   string
}

func (e *HandshakeError) Error() string {
	msg := fmt.Sprintf("websocket: server refused the upgrade with HTTP %d", e.Status)
	if e.Status == http.StatusForbidden || e.Status == http.StatusUnauthorized {
		msg += " - check the token sent in the upgrade request"
	}

	if e.Body != "" {
		msg += ": " + e.Body
	}

	return msg
}

// ReadMessage returns the next complete data message, answering pings and reassembling
// fragments on the way. After the server closes, it returns a *CloseError.
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
			if err := c.writeFrame(opPong, payload); err != nil && !errors.Is(err, ErrClosed) {
				return 0, nil, err
			}

			continue
		case opPong:
			continue
		case opClose:
			return 0, nil, c.onClose(payload)
		case opText, opBinary:
			// A new data frame in the middle of a fragmented one is a protocol violation: the
			// RFC allows only control frames to interleave.
			if inMessage {
				return 0, nil, c.fail(CloseProtocolError, "data frame inside a fragmented message")
			}

			msgType = MessageType(op)
			inMessage = true
		case opContinuation:
			if !inMessage {
				return 0, nil, c.fail(CloseProtocolError, "continuation frame with no message to continue")
			}
		default:
			return 0, nil, c.fail(CloseProtocolError, fmt.Sprintf("unknown opcode %#x", op))
		}

		if int64(len(assembled))+int64(len(payload)) > c.max {
			return 0, nil, c.fail(CloseTooBig, fmt.Sprintf("message over the %d byte limit", c.max))
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

// readFrame reads one frame and applies the per-frame rules. Message-level rules are the
// caller's.
func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, c.readErr(err)
	}

	fin = head[0]&0x80 != 0
	op = head[0] & 0x0f

	// Reserved bits mean an extension, and none was negotiated. Guessing would corrupt the stream.
	if head[0]&0x70 != 0 {
		return false, 0, nil, c.fail(CloseProtocolError, "reserved bits set with no extension negotiated")
	}

	// A server MUST NOT mask. One that does is broken or is not the server we handshook with.
	if head[1]&0x80 != 0 {
		return false, 0, nil, c.fail(CloseProtocolError, "server frame is masked")
	}

	length := uint64(head[1] & 0x7f)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, c.readErr(err)
		}

		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, c.readErr(err)
		}

		length = binary.BigEndian.Uint64(ext[:])
	}

	if op >= opClose {
		// Control frames are small and whole by definition; a large or fragmented one is an
		// attempt to stall the reader between the pieces of a real message.
		if !fin || length > 125 {
			return false, 0, nil, c.fail(CloseProtocolError, "control frame fragmented or over 125 bytes")
		}
	}

	// Checked before allocating: the length is the peer's claim and nothing has arrived to back it.
	if length > uint64(c.max) {
		return false, 0, nil, c.fail(CloseTooBig, fmt.Sprintf("frame declares %d bytes, over the %d limit", length, c.max))
	}

	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, c.readErr(err)
	}

	return fin, op, payload, nil
}

// readErr turns an EOF mid-stream into something that says what happened.
func (c *Conn) readErr(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("websocket: connection dropped without a close frame: %w", io.ErrUnexpectedEOF)
	}

	return err
}

// onClose completes the close handshake the server started and reports it.
func (c *Conn) onClose(payload []byte) error {
	ce := &CloseError{Code: closeNoStatus}

	if len(payload) == 1 {
		_ = c.fail(CloseProtocolError, "close frame with a one-byte payload")

		return ce
	}

	if len(payload) >= 2 {
		ce.Code = int(binary.BigEndian.Uint16(payload[:2]))
		ce.Text = string(payload[2:])
	}

	// Echo the code back, as the RFC asks, then drop the socket: the server closes TCP after
	// seeing our close, and we have nothing left to say.
	echo := payload
	if len(echo) > 2 {
		echo = echo[:2]
	}

	_ = c.writeFrame(opClose, echo)
	_ = c.conn.Close()

	return ce
}

// fail closes the connection with a status and returns the error that caused it.
func (c *Conn) fail(code int, reason string) error {
	_ = c.writeFrame(opClose, closePayload(code, reason))
	_ = c.conn.Close()

	return fmt.Errorf("websocket: %s", reason)
}

// WriteMessage sends one unfragmented data message.
func (c *Conn) WriteMessage(t MessageType, payload []byte) error {
	if t != TextMessage && t != BinaryMessage {
		return fmt.Errorf("websocket: message type %d is not text or binary", t)
	}

	return c.writeFrame(byte(t), payload)
}

// WriteText sends one text message.
func (c *Conn) WriteText(payload []byte) error {
	return c.WriteMessage(TextMessage, payload)
}

// Ping sends a ping. The pong is consumed by ReadMessage.
func (c *Conn) Ping(payload []byte) error {
	if len(payload) > 125 {
		return errors.New("websocket: ping payload over 125 bytes")
	}

	return c.writeFrame(opPing, payload)
}

// closeLinger bounds how long Close waits for the server to finish reading and close its end.
var closeLinger = time.Second

// Close sends a normal close frame, lets the server finish, and closes the socket.
//
// Closing the socket straight after the close frame loses data. When anything the server sent
// is still unread here - a pong, a message nobody read - the kernel answers close() with a RST
// instead of a FIN, and a RST makes the server's kernel throw away whatever it had received but
// the server had not read yet: the tail of what this client wrote, gone although every Write
// succeeded. Linux does this reliably; a test writing 400 messages with pings in between lost
// about a quarter of them. So Close half-closes, then reads and discards until the server closes
// its end (it has read everything by then) or closeLinger passes.
func (c *Conn) Close() error {
	err := c.writeFrame(opClose, closePayload(CloseNormal, ""))
	if err == nil {
		c.linger()
	}

	cerr := c.conn.Close()

	if err != nil && !errors.Is(err, ErrClosed) {
		return err
	}

	if errors.Is(err, ErrClosed) {
		return nil
	}

	return cerr
}

// SetReadDeadline bounds the next ReadMessage.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// writeFrame sends one masked frame. Every client frame is masked; a proxy that checks drops the
// connection otherwise, so this is not optional.
func (c *Conn) writeFrame(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	if c.closed {
		return ErrClosed
	}

	if op == opClose {
		c.closed = true
	}

	head := make([]byte, 0, 14)
	head = append(head, 0x80|op)

	switch n := len(payload); {
	case n < 126:
		head = append(head, 0x80|byte(n))
	case n < 1<<16:
		head = append(head, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(head[2:], uint16(n))
	default:
		head = append(head, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(head[2:], uint64(n))
	}

	var key [4]byte
	if _, err := rand.Read(key[:]); err != nil {
		return fmt.Errorf("websocket: mask key: %w", err)
	}

	head = append(head, key[:]...)

	frame := make([]byte, len(head)+len(payload))
	copy(frame, head)

	body := frame[len(head):]
	for i, b := range payload {
		body[i] = b ^ key[i&3]
	}

	if _, err := c.conn.Write(frame); err != nil {
		return err
	}

	return nil
}

func closePayload(code int, reason string) []byte {
	p := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(p, uint16(code))

	// A close reason is capped by the 125-byte control frame limit.
	if len(reason) > 123 {
		reason = reason[:123]
	}

	return append(p, reason...)
}

// Accept computes Sec-WebSocket-Accept for a key. Exported for test servers.
func Accept(key string) string { return accept(key) }

func accept(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+magic)

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func newKey() string {
	var b [16]byte

	_, _ = rand.Read(b[:])

	return base64.StdEncoding.EncodeToString(b[:])
}

// linger half-closes and drains the socket until the server closes it or closeLinger passes. It
// reads the raw connection, not the buffered reader, so a ReadMessage running concurrently in
// another goroutine is never raced on its buffer; whichever read sees the bytes, they are unread
// data the caller has already given up on.
func (c *Conn) linger() {
	if hc, ok := c.conn.(interface{ CloseWrite() error }); ok {
		_ = hc.CloseWrite()
	}

	_ = c.conn.SetReadDeadline(time.Now().Add(closeLinger))
	_, _ = io.Copy(io.Discard, c.conn)
}
