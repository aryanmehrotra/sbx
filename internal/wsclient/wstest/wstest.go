// Package wstest is a deliberately literal WebSocket server for tests of clients.
//
// It exposes frames rather than messages so a test can send exactly the byte sequence it wants to
// prove the client handles - a fragment, a masked server frame, a ping between fragments - which a
// well-behaved server implementation would refuse to produce. Only test files import it, so none
// of this is linked into the sbx binary.
package wstest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/aryanmehrotra/sbx/internal/wsclient"
)

// Opcodes.
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// Conn is the server side of one upgraded connection.
type Conn struct {
	net.Conn
	br  *bufio.Reader
	wmu sync.Mutex
}

// Upgrade completes the server handshake and hijacks the connection.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "not a websocket upgrade", http.StatusBadRequest)

		return nil, errors.New("not a websocket upgrade")
	}

	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "bad websocket handshake", http.StatusBadRequest)

		return nil, errors.New("bad websocket handshake")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response writer cannot hijack")
	}

	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsclient.Accept(key))
	if err == nil {
		err = rw.Flush()
	}

	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	return &Conn{Conn: conn, br: rw.Reader}, nil
}

// Frame is one frame as read off the wire, already unmasked.
type Frame struct {
	Fin     bool
	Op      byte
	Masked  bool
	Payload []byte
}

// ReadFrame reads one client frame.
func (c *Conn) ReadFrame() (Frame, error) {
	var f Frame

	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		return f, err
	}

	f.Fin = head[0]&0x80 != 0
	f.Op = head[0] & 0x0f
	f.Masked = head[1]&0x80 != 0
	length := uint64(head[1] & 0x7f)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return f, err
		}

		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			return f, err
		}

		length = binary.BigEndian.Uint64(ext[:])
	}

	if length > 256<<20 {
		return f, fmt.Errorf("test server: frame of %d bytes", length)
	}

	var mask [4]byte
	if f.Masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return f, err
		}
	}

	f.Payload = make([]byte, length)
	if _, err := io.ReadFull(c.br, f.Payload); err != nil {
		return f, err
	}

	if f.Masked {
		for i := range f.Payload {
			f.Payload[i] ^= mask[i&3]
		}
	}

	return f, nil
}

// ReadMessage reads client frames until a complete data message, answering pings. It returns
// io.EOF once the client sends a close frame (after echoing it).
func (c *Conn) ReadMessage() (byte, []byte, error) {
	var (
		op  byte
		buf []byte
	)

	for {
		f, err := c.ReadFrame()
		if err != nil {
			return 0, nil, err
		}

		if !f.Masked {
			return 0, nil, errors.New("test server: client frame is not masked")
		}

		switch f.Op {
		case OpPing:
			_ = c.WriteFrame(true, OpPong, f.Payload)

			continue
		case OpPong:
			continue
		case OpClose:
			_ = c.WriteFrame(true, OpClose, f.Payload)

			return 0, nil, io.EOF
		case OpText, OpBinary:
			op = f.Op
		}

		buf = append(buf, f.Payload...)

		if f.Fin {
			return op, buf, nil
		}
	}
}

// WriteFrame sends one unmasked frame with exactly the given FIN bit and opcode.
func (c *Conn) WriteFrame(fin bool, op byte, payload []byte) error {
	return c.writeFrame(fin, op, payload, false, 0)
}

// WriteMaskedFrame sends a frame with the mask bit set, which a server must never do.
func (c *Conn) WriteMaskedFrame(op byte, payload []byte) error {
	return c.writeFrame(true, op, payload, true, 0)
}

// WriteFrameRSV sends a frame with the given reserved bits set.
func (c *Conn) WriteFrameRSV(op byte, payload []byte, rsv byte) error {
	return c.writeFrame(true, op, payload, false, rsv)
}

func (c *Conn) writeFrame(fin bool, op byte, payload []byte, masked bool, rsv byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	b0 := op | rsv<<4
	if fin {
		b0 |= 0x80
	}

	var maskBit byte
	if masked {
		maskBit = 0x80
	}

	head := []byte{b0}

	switch n := len(payload); {
	case n < 126:
		head = append(head, maskBit|byte(n))
	case n < 1<<16:
		head = append(head, maskBit|126, 0, 0)
		binary.BigEndian.PutUint16(head[2:], uint16(n))
	default:
		head = append(head, maskBit|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(head[2:], uint64(n))
	}

	body := payload
	if masked {
		key := [4]byte{1, 2, 3, 4}
		head = append(head, key[:]...)

		body = make([]byte, len(payload))
		for i := range payload {
			body[i] = payload[i] ^ key[i&3]
		}
	}

	_, err := c.Write(append(head, body...))

	return err
}

// WriteText sends one unfragmented text message.
func (c *Conn) WriteText(p []byte) error { return c.WriteFrame(true, OpText, p) }

// CloseWith sends a close frame with a status code and reason.
func (c *Conn) CloseWith(code int, reason string) error {
	p := make([]byte, 2, 2+len(reason))
	binary.BigEndian.PutUint16(p, uint16(code))

	return c.WriteFrame(true, OpClose, append(p, reason...))
}
