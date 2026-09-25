package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

// casePTY is an interactive terminal over execd's PTY WebSocket, the protocol upstream's
// components/execd/PTY.md describes: type into a shell, see its size, resize it, drop the
// connection and come back with ?since= to replay what was missed. The SDK has no PTY client,
// so this speaks the frames directly - which is also what a terminal UI would do.
func casePTY(ctx context.Context, t *T, e *env) {
	sb := e.create(ctx, t, opensandbox.SandboxCreateOptions{})
	defer kill(sb)

	_, base, token, err := execd(ctx, sb)
	t.must(err, "execd endpoint")

	var created struct {
		SessionID string `json:"session_id"`
	}
	t.must(postJSON(ctx, base+"/pty", token, map[string]any{}, &created), "POST /pty")
	t.check(created.SessionID != "", "a PTY session is created (%s)", created.SessionID)

	ws, err := dialWS(ctx, base, "/pty/"+created.SessionID+"/ws", token)
	t.must(err, "open the PTY websocket")

	term := &ptyReader{ws: ws}

	t.check(term.waitText("connected", 10*time.Second), "the server says connected")

	term.send([]byte("stty size\n"))
	t.check(term.waitOutput("24 80", 10*time.Second), "stty size reports the default 24x80")

	t.must(ws.writeText(`{"type":"resize","cols":132,"rows":43}`), "resize")
	term.send([]byte("stty size\n"))
	t.check(term.waitOutput("43 132", 10*time.Second), "after a resize frame, stty size says 43x132")

	marker := "pty-" + randHex(4)
	term.send([]byte("echo " + marker + "-$((6*7))\n"))
	t.check(term.waitOutput(marker+"-42", 10*time.Second), "a command typed into the shell runs and echoes")

	ws.close()

	// Output produced while nobody is attached must be there on reconnect.
	time.Sleep(500 * time.Millisecond)

	var st struct {
		Running      bool  `json:"running"`
		OutputOffset int64 `json:"output_offset"`
	}
	t.must(getJSON(ctx, base+"/pty/"+created.SessionID, token, &st), "GET /pty/{id}")
	t.check(st.Running && st.OutputOffset > 0, "the shell outlives the disconnect (running=%v, offset %d)", st.Running, st.OutputOffset)

	ws2, err := dialWS(ctx, base, "/pty/"+created.SessionID+"/ws?since=0", token)
	t.must(err, "reconnect with since=0")

	term2 := &ptyReader{ws: ws2}
	defer ws2.close()

	t.check(term2.waitOutput(marker+"-42", 10*time.Second), "reconnecting with ?since=0 replays the earlier output")

	term2.send([]byte("echo again-$((1+1))\n"))
	t.check(term2.waitOutput("again-2", 10*time.Second), "and the same shell keeps taking input")

	del, _ := http.NewRequestWithContext(ctx, http.MethodDelete, base+"/pty/"+created.SessionID, nil)
	del.Header.Set("X-EXECD-ACCESS-TOKEN", token)

	if resp, err := http.DefaultClient.Do(del); t.check(err == nil && resp.StatusCode < 300, "DELETE /pty/{id} ends it") {
		resp.Body.Close()
	}
}

// caseProxy is a web server inside the sandbox on a port nobody declared, reached from here
// through GetEndpoint - the /proxy/{port} route.
func caseProxy(ctx context.Context, t *T, e *env) {
	sb := e.create(ctx, t, opensandbox.SandboxCreateOptions{})
	defer kill(sb)

	marker := "served-" + randHex(4)
	_, _, _, err := run(ctx, sb, "mkdir -p /srv/www && echo "+marker+" > /srv/www/index.html")
	t.must(err, "write index.html")

	_, err = sb.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{
		Command: "cd /srv/www && python -m http.server 8000", Background: true,
	}, nil)
	t.must(err, "start http.server")

	ep, err := sb.GetEndpoint(ctx, 8000)
	t.must(err, "GetEndpoint(8000)")
	t.check(strings.Contains(ep.Endpoint, "/proxy/8000"), "an undeclared port resolves to the /proxy/8000 route (%s)", ep.Endpoint)

	u := ep.Endpoint
	if !strings.HasPrefix(u, "http") {
		u = "http://" + u
	}

	var body string
	var status int

	got := waitFor(ctx, 20*time.Second, 300*time.Millisecond, func() bool {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u+"/index.html", nil)
		for k, v := range ep.Headers {
			req.Header.Set(k, v)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()

		b, _ := io.ReadAll(resp.Body)
		body, status = string(b), resp.StatusCode

		return status == 200 && strings.Contains(body, marker)
	})
	t.check(got, "GET through the endpoint reaches the server in the sandbox (%d %q)", status, clip(body))

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u+"/index.html", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.check(resp.StatusCode == 401 || resp.StatusCode == 403, "without the endpoint's headers the proxy refuses (%d)", resp.StatusCode)
	} else {
		t.ok("without the endpoint's headers the proxy refuses (%v)", err)
	}

	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, strings.Replace(u, "/proxy/8000", "/proxy/8001", 1)+"/", nil)
	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}

	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
		t.check(resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504, "a port with nothing listening is a 5xx, not a hang (%d)", resp.StatusCode)
	} else {
		t.fail("a port with nothing listening: %v", err)
	}
}

func postJSON(ctx context.Context, u, token string, in, out any) error {
	b, _ := json.Marshal(in)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-EXECD-ACCESS-TOKEN", token)

	return doJSON(req, out)
}

func getJSON(ctx context.Context, u, token string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("X-EXECD-ACCESS-TOKEN", token)

	return doJSON(req, out)
}

func doJSON(req *http.Request, out any) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, clip(string(b)))
	}

	return json.Unmarshal(b, out)
}

// ptyReader accumulates what the shell printed, from live 0x01/0x02 frames and 0x03 replay
// frames alike, and the JSON control messages separately.
type ptyReader struct {
	ws   *wsConn
	out  bytes.Buffer
	text bytes.Buffer
}

func (p *ptyReader) send(b []byte) { _ = p.ws.writeBinary(append([]byte{0x00}, b...)) }

func (p *ptyReader) waitOutput(want string, within time.Duration) bool {
	return p.pump(within, func() bool { return strings.Contains(p.out.String(), want) })
}

func (p *ptyReader) waitText(want string, within time.Duration) bool {
	return p.pump(within, func() bool { return strings.Contains(p.text.String(), want) })
}

func (p *ptyReader) pump(within time.Duration, done func() bool) bool {
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			return false
		}

		_ = p.ws.c.SetReadDeadline(deadline)

		op, data, err := p.ws.read()
		if err != nil {
			return done()
		}

		switch {
		case op == 1:
			p.text.Write(data)
		case op == 2 && len(data) > 0 && (data[0] == 0x01 || data[0] == 0x02):
			p.out.Write(data[1:])
		case op == 2 && len(data) > 9 && data[0] == 0x03:
			p.out.Write(data[9:])
		}
	}

	return true
}

// wsConn is the least WebSocket client that works: RFC 6455 handshake, masked client frames,
// unfragmented server frames, and pings answered.
type wsConn struct {
	c net.Conn
	r *bufio.Reader
}

func dialWS(ctx context.Context, base, path, token string) (*wsConn, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}

	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}

	keyRaw := make([]byte, 16)
	_, _ = rand.Read(keyRaw)
	key := base64.StdEncoding.EncodeToString(keyRaw)

	fullPath := strings.TrimSuffix(u.Path, "/") + path
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nX-EXECD-ACCESS-TOKEN: %s\r\n\r\n",
		fullPath, u.Host, key, token)

	r := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))

	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		c.Close()
		return nil, err
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		c.Close()

		return nil, fmt.Errorf("websocket upgrade: %d %s", resp.StatusCode, clip(string(b)))
	}

	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(h[:]) {
		c.Close()
		return nil, fmt.Errorf("websocket upgrade: bad Sec-WebSocket-Accept")
	}

	return &wsConn{c: c, r: r}, nil
}

func (w *wsConn) writeText(s string) error   { return w.write(1, []byte(s)) }
func (w *wsConn) writeBinary(b []byte) error { return w.write(2, b) }
func (w *wsConn) close()                     { _ = w.write(8, []byte{0x03, 0xe8}); _ = w.c.Close() }

func (w *wsConn) write(op byte, payload []byte) error {
	var hdr []byte
	hdr = append(hdr, 0x80|op)

	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n < 1<<16:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}

	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	hdr = append(hdr, mask...)

	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}

	_ = w.c.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := w.c.Write(append(hdr, masked...))

	return err
}

func (w *wsConn) read() (byte, []byte, error) {
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(w.r, h); err != nil {
			return 0, nil, err
		}

		op := h[0] & 0x0f
		n := uint64(h[1] & 0x7f)

		switch n {
		case 126:
			b := make([]byte, 2)
			if _, err := io.ReadFull(w.r, b); err != nil {
				return 0, nil, err
			}

			n = uint64(binary.BigEndian.Uint16(b))
		case 127:
			b := make([]byte, 8)
			if _, err := io.ReadFull(w.r, b); err != nil {
				return 0, nil, err
			}

			n = binary.BigEndian.Uint64(b)
		}

		data := make([]byte, n)
		if _, err := io.ReadFull(w.r, data); err != nil {
			return 0, nil, err
		}

		switch op {
		case 9:
			_ = w.write(10, data)
			continue
		case 10:
			continue
		case 8:
			return op, data, io.EOF
		}

		return op, data, nil
	}
}
