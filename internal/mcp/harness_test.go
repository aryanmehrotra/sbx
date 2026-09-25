package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/osbclient"
	"github.com/aryanmehrotra/sbx/internal/osbclient/osbtest"
)

// client drives a Server over in-memory pipes the way an MCP client drives `sbx mcp` over
// stdio: one JSON object per line each way.
type client struct {
	t   *testing.T
	in  *io.PipeWriter
	mu  sync.Mutex
	got []map[string]any // every line the server wrote, in order
	raw []string
	cnd *sync.Cond
	id  int
	eof bool
}

func startServer(t *testing.T, s *Server) *client {
	t.Helper()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	c := &client{t: t, in: inW}
	c.cnd = sync.NewCond(&c.mu)

	done := make(chan error, 1)

	go func() {
		done <- s.Serve(context.Background(), inR, outW)
		_ = outW.Close()
	}()

	go func() {
		br := bufio.NewReader(outR)

		for {
			line, err := br.ReadString('\n')
			if line != "" {
				var m map[string]any

				// Every line on stdout must be one JSON-RPC message. Anything else - a log
				// line, a blank line - breaks a real client, so it fails the test here.
				// A batch answer is an array of them.
				if jerr := json.Unmarshal([]byte(line), &m); jerr != nil {
					var batch []map[string]any
					if json.Unmarshal([]byte(line), &batch) != nil || len(batch) == 0 {
						t.Errorf("server wrote a line that is not JSON-RPC: %q", line)
					}
				}

				c.mu.Lock()
				c.got = append(c.got, m)
				c.raw = append(c.raw, line)
				c.cnd.Broadcast()
				c.mu.Unlock()
			}

			if err != nil {
				c.mu.Lock()
				c.eof = true
				c.cnd.Broadcast()
				c.mu.Unlock()

				return
			}
		}
	}()

	t.Cleanup(func() {
		_ = inW.Close()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v at end of input", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after its input closed")
		}
	})

	return c
}

func (c *client) send(v any) {
	c.t.Helper()

	var data []byte

	switch x := v.(type) {
	case string:
		data = []byte(x)
	default:
		data, _ = json.Marshal(x)
	}

	if _, err := c.in.Write(append(data, '\n')); err != nil {
		c.t.Fatalf("writing to the server: %v", err)
	}
}

// request sends a request and returns its id.
func (c *client) request(method string, params any) int {
	c.mu.Lock()
	c.id++
	id := c.id
	c.mu.Unlock()

	m := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		m["params"] = params
	}

	c.send(m)

	return id
}

// await waits for the answer to id.
func (c *client) await(id int) map[string]any {
	c.t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	c.mu.Lock()
	defer c.mu.Unlock()

	for {
		for _, m := range c.got {
			if n, ok := m["id"].(float64); ok && int(n) == id {
				return m
			}
		}

		if c.eof || time.Now().After(deadline) {
			c.t.Fatalf("no answer to request %d; the server wrote %v", id, c.raw)
		}

		go func() { time.Sleep(20 * time.Millisecond); c.cnd.Broadcast() }()
		c.cnd.Wait()
	}
}

func (c *client) call(method string, params any) map[string]any {
	c.t.Helper()

	return c.await(c.request(method, params))
}

// lines returns everything written so far.
func (c *client) lines() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]map[string]any(nil), c.got...)
}

func (c *client) initialize(version string) map[string]any {
	c.t.Helper()

	r := c.call("initialize", map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	return r
}

// tool calls a tool and returns its result object, failing on a protocol error.
func (c *client) tool(name string, args any) map[string]any {
	c.t.Helper()

	r := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if r["error"] != nil {
		c.t.Fatalf("%s: protocol error %v", name, r["error"])
	}

	return r["result"].(map[string]any)
}

// ok calls a tool that must succeed and returns its decoded text content.
func (c *client) ok(name string, args any) any {
	c.t.Helper()

	res := c.tool(name, args)
	if res["isError"] == true {
		c.t.Fatalf("%s(%v) failed: %v", name, args, text(res))
	}

	var v any
	if err := json.Unmarshal([]byte(text(res)), &v); err != nil {
		c.t.Fatalf("%s: text content is not JSON: %q", name, text(res))
	}

	return v
}

// fails calls a tool that must fail as a tool (isError), and returns the message.
func (c *client) fails(name string, args any) string {
	c.t.Helper()

	res := c.tool(name, args)
	if res["isError"] != true {
		c.t.Fatalf("%s(%v) succeeded, want isError: %v", name, args, text(res))
	}

	return text(res)
}

func text(res map[string]any) string {
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return ""
	}

	first, _ := content[0].(map[string]any)
	s, _ := first["text"].(string)

	return s
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)

	return m
}

// sandboxServer is a Server with the sandbox tools, pointed at a fresh fake.
func sandboxServer(t *testing.T) (*osbtest.Server, *client) {
	t.Helper()

	f := osbtest.New()
	f.Key = "k"
	t.Cleanup(f.Close)

	oc, err := osbclient.New(f.URL, "k")
	if err != nil {
		t.Fatal(err)
	}

	s := NewServer("sbx", "test")
	s.Instructions = Instructions

	var logs strings.Builder

	s.Log = &syncWriter{w: &logs}
	s.Register(NewSandboxes(oc).Tools()...)

	c := startServer(t, s)
	c.initialize("2025-06-18")

	return f, c
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.w.Write(p)
}

func must(t *testing.T, cond bool, format string, args ...any) {
	t.Helper()

	if !cond {
		t.Errorf(format, args...)
	}
}
