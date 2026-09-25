package mcp

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"
)

func echoServer() *Server {
	s := NewServer("sbx", "1.2.3")
	s.Register(
		Tool{
			Name: "echo", Description: "echo", InputSchema: object(map[string]schema{"x": str("")}),
			Handler: func(_ context.Context, c *Call) (any, error) {
				var a struct {
					X string `json:"x"`
				}
				if err := c.Bind(&a); err != nil {
					return nil, err
				}

				return map[string]string{"x": a.X}, nil
			},
		},
		Tool{
			Name: "list", Description: "a list result", InputSchema: object(nil),
			Handler: func(context.Context, *Call) (any, error) { return []int{1, 2}, nil },
		},
		Tool{
			Name: "boom", Description: "fails", InputSchema: object(nil),
			Handler: func(context.Context, *Call) (any, error) { return nil, errors.New("it broke; do X next") },
		},
		Tool{
			Name: "panics", Description: "panics", InputSchema: object(nil),
			Handler: func(context.Context, *Call) (any, error) { panic("nil map") },
		},
		Tool{
			Name: "block", Description: "blocks until cancelled", InputSchema: object(nil),
			Handler: func(ctx context.Context, c *Call) (any, error) {
				c.Progress(1, 2, "waiting")
				<-ctx.Done()

				return nil, ctx.Err()
			},
		},
	)

	return s
}

func errCode(r map[string]any) int {
	e, _ := r["error"].(map[string]any)
	n, _ := e["code"].(float64)

	return int(n)
}

func TestInitializeNegotiatesTheVersion(t *testing.T) {
	for asked, want := range map[string]string{
		"2025-06-18": "2025-06-18",
		"2025-03-26": "2025-03-26",
		"2024-11-05": "2024-11-05",
		"2025-11-25": "2025-11-25",
		"1999-01-01": ProtocolVersions[0], // unknown: offer our newest, the client decides
	} {
		c := startServer(t, echoServer())
		r := c.initialize(asked)
		res := obj(r["result"])

		must(t, res["protocolVersion"] == want, "asked %s, got %v, want %s", asked, res["protocolVersion"], want)
		must(t, obj(obj(res["capabilities"])["tools"]) != nil, "capabilities.tools missing: %v", res)
		must(t, obj(res["serverInfo"])["name"] == "sbx" && obj(res["serverInfo"])["version"] == "1.2.3",
			"serverInfo = %v", res["serverInfo"])
	}
}

func TestInitializeWithoutAVersionIsInvalidParams(t *testing.T) {
	c := startServer(t, echoServer())

	r := c.call("initialize", map[string]any{})
	must(t, errCode(r) == codeInvalidParams, "got %v, want -32602", r)
}

func TestProtocolErrors(t *testing.T) {
	c := startServer(t, echoServer())
	c.initialize("2025-06-18")

	t.Run("malformed JSON is a parse error with a null id", func(t *testing.T) {
		before := len(c.lines())
		c.send(`{"jsonrpc":"2.0","id":7,"method":`)

		deadline := time.Now().Add(5 * time.Second)
		for len(c.lines()) == before && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}

		got := c.lines()
		if len(got) == before {
			t.Fatal("no answer to malformed input")
		}

		last := got[len(got)-1]
		must(t, errCode(last) == codeParse && last["id"] == nil, "malformed input answered %v", last)

		// And the server is still there.
		must(t, obj(c.call("ping", nil)["result"]) != nil, "ping after a parse error failed")
	})

	t.Run("unknown method", func(t *testing.T) {
		must(t, errCode(c.call("resources/list", nil)) == codeMethodNotFound, "unknown method not -32601")
	})

	t.Run("wrong jsonrpc version", func(t *testing.T) {
		c.send(map[string]any{"jsonrpc": "1.0", "id": 900, "method": "ping"})
		must(t, errCode(c.await(900)) == codeInvalidRequest, "jsonrpc 1.0 not -32600")
	})

	t.Run("unknown tool is a protocol error", func(t *testing.T) {
		r := c.call("tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}})
		must(t, errCode(r) == codeInvalidParams, "unknown tool answered %v, want -32602", r)
	})

	t.Run("a failing tool is a result with isError, not a protocol error", func(t *testing.T) {
		r := c.call("tools/call", map[string]any{"name": "boom"})
		must(t, r["error"] == nil, "tool failure became a protocol error: %v", r)
		res := obj(r["result"])
		must(t, res["isError"] == true && text(res) == "it broke; do X next", "result = %v", res)
	})

	t.Run("a panicking tool is a tool error and the server survives", func(t *testing.T) {
		res := c.tool("panics", nil)
		must(t, res["isError"] == true, "panic result = %v", res)
		must(t, obj(c.call("ping", nil)["result"]) != nil, "server died with the panic")
	})

	t.Run("a misspelt argument is a tool error the model can read", func(t *testing.T) {
		res := c.tool("echo", map[string]any{"y": "1"})
		must(t, res["isError"] == true, "unknown argument accepted: %v", res)
	})

	t.Run("a notification gets no answer", func(t *testing.T) {
		before := len(c.lines())
		c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/whatever"})
		c.call("ping", nil)
		must(t, len(c.lines()) == before+1, "a notification was answered: %v", c.lines()[before:])
	})
}

func TestToolsListAndResults(t *testing.T) {
	c := startServer(t, echoServer())
	c.initialize("2025-06-18")

	tools, _ := obj(c.call("tools/list", nil)["result"])["tools"].([]any)
	if len(tools) != 5 {
		t.Fatalf("tools/list = %v", tools)
	}

	for _, x := range tools {
		tl := obj(x)
		must(t, tl["name"] != "" && obj(tl["inputSchema"])["type"] == "object", "tool %v lacks a name or object schema", tl)
	}

	res := c.tool("echo", map[string]any{"x": "hi"})
	must(t, text(res) == `{"x":"hi"}`, "text = %q", text(res))
	must(t, obj(res["structuredContent"])["x"] == "hi", "structuredContent = %v", res["structuredContent"])

	// A list is wrapped, because structuredContent must be an object.
	res = c.tool("list", nil)
	sc := obj(res["structuredContent"])
	must(t, sc != nil && len(sc["result"].([]any)) == 2, "list structuredContent = %v", res["structuredContent"])
}

func TestStructuredContentOnlyWhereTheVersionHasIt(t *testing.T) {
	c := startServer(t, echoServer())
	c.initialize("2025-03-26")

	res := c.tool("echo", map[string]any{"x": "hi"})
	must(t, res["structuredContent"] == nil, "2025-03-26 got structuredContent: %v", res)
	must(t, text(res) == `{"x":"hi"}`, "text = %q", text(res))
}

func TestBatch(t *testing.T) {
	c := startServer(t, echoServer())
	c.initialize("2025-03-26")

	before := len(c.lines())
	c.send(`[{"jsonrpc":"2.0","id":501,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/x"},{"jsonrpc":"2.0","id":502,"method":"nope"}]`)

	deadline := time.Now().Add(5 * time.Second)
	for len(c.lines()) == before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	c.mu.Lock()
	raw := c.raw[len(c.raw)-1]
	c.mu.Unlock()

	if raw[0] != '[' {
		t.Fatalf("a batch was not answered with an array: %s", raw)
	}

	for _, id := range []string{`"id":501`, `"id":502`} {
		if !strings.Contains(raw, id) {
			t.Errorf("batch answer %s lacks %s", raw, id)
		}
	}
}

func TestCancellationAndConcurrency(t *testing.T) {
	c := startServer(t, echoServer())
	c.initialize("2025-06-18")

	id := c.request("tools/call", map[string]any{"name": "block", "_meta": map[string]any{"progressToken": "p1"}})

	// The blocked call must not stop the server answering others.
	must(t, obj(c.call("ping", nil)["result"]) != nil, "ping blocked behind a running tool")

	// It asked for progress, so a progress notification carries its token. The tool runs
	// concurrently with the ping, so the notification may land after the ping's answer.
	var sawProgress bool

	for deadline := time.Now().Add(5 * time.Second); !sawProgress && time.Now().Before(deadline); {
		for _, m := range c.lines() {
			if m["method"] == "notifications/progress" && obj(m["params"])["progressToken"] == "p1" {
				sawProgress = true
			}
		}

		time.Sleep(2 * time.Millisecond)
	}

	must(t, sawProgress, "no notifications/progress for the token: %v", c.lines())

	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": id, "reason": "user"}})

	// A cancelled request gets no answer; wait long enough that one would have arrived.
	c.call("ping", nil)
	time.Sleep(50 * time.Millisecond)

	for _, m := range c.lines() {
		if n, ok := m["id"].(float64); ok && int(n) == id {
			t.Fatalf("cancelled request %d was answered: %v", id, m)
		}
	}
}

func TestToolsAreSortedAndUnique(t *testing.T) {
	s := echoServer()

	var names []string
	for _, tl := range s.Tools() {
		names = append(names, tl.Name)
	}

	must(t, sort.StringsAreSorted(names), "Tools() not sorted: %v", names)
}

// End of input is the client hanging up, but what it already sent is still answered - which is
// what makes `printf '...' | sbx mcp` usable, and what a client that closes stdin right after
// its last request expects.
func TestEndOfInputStillAnswersWhatWasRead(t *testing.T) {
	s := echoServer()
	s.Register(Tool{
		Name: "slow", Description: "takes a moment", InputSchema: object(nil),
		Handler: func(ctx context.Context, _ *Call) (any, error) {
			select {
			case <-time.After(100 * time.Millisecond):
				return map[string]bool{"done": true}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	})

	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow"}}` + "\n")

	var out strings.Builder

	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), `"id":2,"result"`) || !strings.Contains(out.String(), `{\"done\":true}`) {
		t.Fatalf("the request read before end of input was not answered:\n%s", out.String())
	}
}

// A cancellation written in the same breath as its request must still find it. Were requests
// registered from their own goroutine, the notice could be read first and lost - and the
// tool would run on, holding Serve open past the end of input (the cleanup would then fail).
func TestCancellationRightBehindTheRequest(t *testing.T) {
	for i := 0; i < 20; i++ {
		c := startServer(t, echoServer())
		c.initialize("2025-06-18")

		c.send(`{"jsonrpc":"2.0","id":"r1","method":"tools/call","params":{"name":"block"}}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"r1"}}`)
		c.call("ping", nil)

		for _, m := range c.lines() {
			if m["id"] == "r1" {
				t.Fatalf("cancelled request answered: %v", m)
			}
		}
	}
}
