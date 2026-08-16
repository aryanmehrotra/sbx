package daemon

// The WebSocket subset this needs, and no more.
//
// Why hand-written: the root module has no dependencies outside the standard library, and that
// is a claim gated in CI rather than a preference. Why WebSocket at all: a deployed sandbox is
// reached through whatever single HTTPS endpoint the platform gives you, and a layer 7 proxy
// terminates TLS - which strips ALPN and refuses CONNECT, while passing WebSocket through
// untouched because every ordinary application needs it. Teleport reached the same conclusion
// for the same reason after trying ALPN first.
//
// This is a *server* subset: accept a handshake, read masked binary frames from a client, write
// unmasked ones back, answer a ping, and close cleanly. It does not do compression, extensions,
// subprotocols, or the client half of the handshake beyond what `sbx connect` needs.
//
// The parser is reachable by anything holding the token, so it is attack surface: every length
// form, a declared length far larger than what arrives, an unmasked client frame and a reserved
// bit are all cases with tests rather than assumptions.

import (
	"bufio"
	"crypto/rand"
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
)

// wsMagic is the constant RFC 6455 appends to the client key before hashing. It exists so that
// a cache or a proxy cannot accidentally complete a handshake it did not understand.
const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxFrame caps a single frame's payload. A client declares its own length before sending any
// of it, so without a ceiling one 8-byte header can ask for an allocation of terabytes.
// Relayed TCP is read in 32 KiB chunks, so this is generous by two orders of magnitude and
// still refuses the attack.
const maxFrame = 4 << 20

// Opcodes, of which this needs five.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

var errClosed = errors.New("websocket closed")

// wsConn is one upgraded connection.
//
// Writes go through a mutex because two goroutines want to write: the relay carrying bytes from
// the sandbox, and the keepalive ping. net.Conn.Write is not safe for concurrent use, and two
// interleaved frame headers corrupt the stream in a way that looks like a protocol bug
// somewhere else entirely.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	// mask is set on the client side. RFC 6455 requires a client to mask every frame and a
	// server never to, and a proxy that checks will drop the connection if either gets it
	// wrong - so this is not decoration.
	mask bool

	wmu sync.Mutex

	// pong is closed-over state for the keepalive: the read side records when the peer last
	// answered, and the pinger gives up when the answer stops coming. A ping with nothing
	// checking for a pong is decoration - it detects none of the half-open connections it
	// exists for, and leaves both relay goroutines alive forever.
	pmu      sync.Mutex
	lastPong time.Time
}

// wsAccept computes the handshake response for a client key.
func wsAccept(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+wsMagic)

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsUpgrade completes the server side of the handshake and hijacks the connection.
//
// The checks are the ones RFC 6455 makes mandatory. A request that fails any of them is a
// client that will not understand what we send next, so it gets an ordinary HTTP error rather
// than a half-built socket.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return nil, errors.New("not a websocket upgrade")
	}

	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, errors.New("unsupported websocket version")
	}

	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("connection cannot be hijacked")
	}

	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	// The server's own deadlines are inherited by the hijacked connection, and a tunnel
	// outlives any of them. Cleared here rather than by configuring the server, so that the
	// only connections without a write deadline are the ones deliberately upgraded.
	_ = conn.SetDeadline(time.Time{})

	_, err = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", wsAccept(key))
	if err == nil {
		err = rw.Flush()
	}

	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	c := &wsConn{conn: conn, br: rw.Reader}
	c.lastPong = time.Now()

	return c, nil
}

// readFrame returns the next data frame's payload, answering control frames itself.
//
// Fragmentation is handled by reassembly rather than streamed, because the caller relays whole
// chunks and a partial one is not useful to it. Control frames are legal *between* fragments,
// which is why they are answered inside this loop rather than before it.
func (c *wsConn) readFrame() ([]byte, error) {
	var assembled []byte

	for {
		final, opcode, payload, err := c.readOne()
		if err != nil {
			return nil, err
		}

		switch opcode {
		case opClose:
			return nil, errClosed
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return nil, err
			}

			continue
		case opPong:
			c.pmu.Lock()
			c.lastPong = time.Now()
			c.pmu.Unlock()

			continue
		case opBinary, opText, opContinuation:
			assembled = append(assembled, payload...)
			if len(assembled) > maxFrame {
				return nil, errors.New("fragmented message over the frame ceiling")
			}

			if final {
				return assembled, nil
			}

			continue
		default:
			return nil, fmt.Errorf("unknown opcode %#x", opcode)
		}
	}
}

// readOne reads exactly one frame off the wire.
func (c *wsConn) readOne() (final bool, opcode byte, payload []byte, err error) {
	var head [2]byte

	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return false, 0, nil, err
	}

	final = head[0]&0x80 != 0

	// Reserved bits mean an extension was negotiated. None was, so a peer setting them is
	// speaking a protocol this does not implement and guessing would corrupt the stream.
	if head[0]&0x70 != 0 {
		return false, 0, nil, errors.New("reserved bits set with no extension negotiated")
	}

	opcode = head[0] & 0x0f
	masked := head[1]&0x80 != 0
	length := uint64(head[1] & 0x7f)

	// A client MUST mask. An unmasked client frame is either a broken implementation or an
	// attempt to smuggle content past a proxy that is looking at plaintext, and the RFC
	// requires the server to fail the connection rather than accommodate it.
	if !masked {
		return false, 0, nil, errors.New("client frame is not masked")
	}

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

	// Checked before allocating, which is the whole point: the length is the peer's claim and
	// nothing has arrived yet to corroborate it.
	if length > maxFrame {
		return false, 0, nil, fmt.Errorf("frame declares %d bytes, over the %d ceiling", length, maxFrame)
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

	return final, opcode, payload, nil
}

// write sends one unmasked frame. Server frames are never masked, which the RFC requires in
// the other direction for the same reason it requires masking in this one.
func (c *wsConn) write(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()

	head := []byte{0x80 | opcode}

	maskBit := byte(0)
	if c.mask {
		maskBit = 0x80
	}

	switch n := len(payload); {
	case n < 126:
		head = append(head, byte(n)|maskBit)
	case n < 1<<16:
		head = append(head, 126|maskBit, 0, 0)
		binary.BigEndian.PutUint16(head[2:], uint16(n))
	default:
		head = append(head, 127|maskBit, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(head[2:], uint64(n))
	}

	body := payload

	if c.mask {
		var key [4]byte

		if _, err := rand.Read(key[:]); err != nil {
			return err
		}

		head = append(head, key[:]...)

		body = make([]byte, len(payload))
		for i := range payload {
			body[i] = payload[i] ^ key[i%4]
		}
	}

	if _, err := c.conn.Write(append(head, body...)); err != nil {
		return err
	}

	return nil
}

// readServerFrame is the client's read: the mirror of readOne, for frames a server sent, which
// are never masked.
func (c *wsConn) readServerFrame() ([]byte, error) {
	var assembled []byte

	for {
		var head [2]byte
		if _, err := io.ReadFull(c.br, head[:]); err != nil {
			return nil, err
		}

		final := head[0]&0x80 != 0
		opcode := head[0] & 0x0f
		length := uint64(head[1] & 0x7f)

		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}

			length = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}

			length = binary.BigEndian.Uint64(ext[:])
		}

		if length > maxFrame {
			return nil, fmt.Errorf("server frame declares %d bytes", length)
		}

		payload := make([]byte, length)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}

		switch opcode {
		case opClose:
			return nil, errClosed
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return nil, err
			}

			continue
		case opPong:
			c.pmu.Lock()
			c.lastPong = time.Now()
			c.pmu.Unlock()

			continue
		}

		assembled = append(assembled, payload...)
		if final {
			return assembled, nil
		}
	}
}

// keepalive pings until the peer stops answering.
//
// The deadline is what makes this useful. An L7 proxy that drops a connection without a FIN
// leaves a socket that reads and writes forever without reaching anything, which is exactly
// the case a keepalive exists to notice - and noticing requires checking that the pong came
// back, not merely that the ping went out.
func (c *wsConn) keepalive(every, deadline time.Duration, done <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()

	for {
		select {
		case <-done:
			return
		case <-t.C:
			c.pmu.Lock()
			silent := time.Since(c.lastPong)
			c.pmu.Unlock()

			if silent > deadline {
				_ = c.conn.Close() // unblocks both relays, which then unwind

				return
			}

			if err := c.write(opPing, nil); err != nil {
				return
			}
		}
	}
}

func (c *wsConn) close() error {
	_ = c.write(opClose, nil)

	return c.conn.Close()
}

// wsKey makes a client key. Used by `sbx connect`; the value is never a secret, it only has to
// be fresh so that a cached 101 cannot be replayed at us.
func wsKey() string {
	var b [16]byte

	_, _ = rand.Read(b[:])

	return base64.StdEncoding.EncodeToString(b[:])
}
