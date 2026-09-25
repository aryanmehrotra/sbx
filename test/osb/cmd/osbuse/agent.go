package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

// caseMCP is an agent that has only `sbx mcp`: JSON-RPC over the child's stdin and stdout, the
// way Claude Code or any MCP host drives it, and nothing else. No SDK is involved on this side.
func caseMCP(ctx context.Context, t *T, e *env) {
	cmd := exec.CommandContext(ctx, e.arg("sbx"), "mcp", "--url", e.url, "--key", e.key)
	stdin, err := cmd.StdinPipe()
	t.must(err, "stdin pipe")
	stdout, err := cmd.StdoutPipe()
	t.must(err, "stdout pipe")

	var stderr strings.Builder
	cmd.Stderr = &stderr
	t.must(cmd.Start(), "start sbx mcp")

	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()

	c := &mcpClient{in: stdin, out: bufio.NewReader(stdout)}

	init, err := c.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "osbuse", "version": "0"},
	})
	t.must(err, "initialize")
	t.check(strings.Contains(string(init), `"tools"`), "initialize advertises the tools capability")
	t.must(c.notify("notifications/initialized"), "initialized")

	list, err := c.call("tools/list", map[string]any{})
	t.must(err, "tools/list")

	var tl struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	_ = json.Unmarshal(list, &tl)
	t.check(len(tl.Tools) == 19, "tools/list has the 19 tools README promises (got %d)", len(tl.Tools))

	created, isErr, err := c.tool("sandbox_create", map[string]any{"image": e.image, "timeout_seconds": 300})
	t.must(err, "sandbox_create")

	var cr struct {
		SandboxID string `json:"sandbox_id"`
	}
	_ = json.Unmarshal([]byte(created), &cr)
	if !t.check(!isErr && strings.HasPrefix(cr.SandboxID, "osb-"), "sandbox_create returns a sandbox_id (%s)", cr.SandboxID) {
		t.fail("sandbox_create said: %s", clip(created))
		return
	}

	id := cr.SandboxID
	killed := false

	defer func() {
		if !killed {
			_, _, _ = c.tool("sandbox_kill", map[string]any{"sandbox_id": id})
		}
	}()

	out, isErr, err := c.tool("command_run", map[string]any{"sandbox_id": id, "command": `python -c 'print(6*7)'`})
	t.check(err == nil && !isErr && strings.Contains(out, "42"), "command_run python -c prints 42: %s", clip(out))

	const src = "import sys\nprint('hello from', sys.version_info[0])\n"
	_, isErr, err = c.tool("file_write", map[string]any{"sandbox_id": id, "path": "/work/agent/hello.py", "content": src})
	t.check(err == nil && !isErr, "file_write creates /work/agent/hello.py (and its directory)")

	read, isErr, err := c.tool("file_read", map[string]any{"sandbox_id": id, "path": "/work/agent/hello.py"})

	var rd struct{ Content string }
	_ = json.Unmarshal([]byte(read), &rd)
	t.check(err == nil && !isErr && rd.Content == src, "file_read returns exactly what was written")

	found, isErr, err := c.tool("file_search", map[string]any{"sandbox_id": id, "path": "/work", "pattern": "*.py"})
	t.check(err == nil && !isErr && strings.Contains(found, "/work/agent/hello.py"), "file_search finds it by glob: %s", clip(found))

	out, isErr, err = c.tool("command_run", map[string]any{"sandbox_id": id, "command": "python /work/agent/hello.py"})
	t.check(err == nil && !isErr && strings.Contains(out, "hello from 3"), "the written file runs: %s", clip(out))

	out, isErr, err = c.tool("command_run", map[string]any{"sandbox_id": id, "command": "exit 3"})
	t.check(err == nil && strings.Contains(strings.ReplaceAll(out, " ", ""), `"exit_code":3`), "a failing command reports its exit code: %s", clip(out))

	_, isErr, err = c.tool("sandbox_kill", map[string]any{"sandbox_id": id})
	killed = t.check(err == nil && !isErr, "sandbox_kill")

	info, isErr, err := c.tool("sandbox_get_info", map[string]any{"sandbox_id": id})
	t.check(err == nil && isErr, "a killed sandbox is gone to the agent too: %s", clip(info))

	lifecycle := opensandbox.NewLifecycleClient(e.url+"/v1", e.key)
	_, gerr := lifecycle.GetSandbox(ctx, id)
	t.check(gerr != nil, "and to the lifecycle API")
}

type mcpClient struct {
	mu  sync.Mutex
	in  io.Writer
	out *bufio.Reader
	id  int
}

func (c *mcpClient) notify(method string) error {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	_, err := c.in.Write(append(b, '\n'))

	return err
}

func (c *mcpClient) call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.id++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})

	if _, err := c.in.Write(append(b, '\n')); err != nil {
		return nil, err
	}

	for {
		line, err := c.out.ReadBytes('\n')
		if err != nil {
			return nil, fmt.Errorf("reading the reply to %s: %w", method, err)
		}

		var resp struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int
				Message string
			} `json:"error"`
		}

		if json.Unmarshal(line, &resp) != nil || resp.ID == nil || *resp.ID != c.id {
			continue // a notification or log line; not our answer
		}

		if resp.Error != nil {
			return nil, fmt.Errorf("%s: %d %s", method, resp.Error.Code, resp.Error.Message)
		}

		return resp.Result, nil
	}
}

// tool calls one tool and returns its text content (the JSON result), and whether the server
// marked it as an error.
func (c *mcpClient) tool(name string, args map[string]any) (string, bool, error) {
	raw, err := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", false, err
	}

	var r struct {
		Content []struct{ Text string } `json:"content"`
		IsError bool                    `json:"isError"`
	}

	if err := json.Unmarshal(raw, &r); err != nil {
		return "", false, err
	}

	var b strings.Builder
	for _, p := range r.Content {
		b.WriteString(p.Text)
	}

	return b.String(), r.IsError, nil
}

func clip(s string) string {
	s = strings.ReplaceAll(s, "\n", `\n`)
	if len(s) > 160 {
		return s[:160] + "..."
	}

	return s
}

// caseCoding is the loop a coding agent runs through the SDK: write code, run it, stream its
// output as it happens, start a server-ish process in the background, read its logs, and stop it.
func caseCoding(ctx context.Context, t *T, e *env) {
	sb := e.create(ctx, t, opensandbox.SandboxCreateOptions{})
	defer kill(sb)

	const prog = "import time\nfor i in range(3):\n    print('tick', i, flush=True)\n    time.sleep(0.4)\n"
	err := sb.UploadFile(ctx, strings.NewReader(prog), opensandbox.UploadFileOptions{
		FileName: "main.py", Metadata: opensandbox.FileMetadata{Path: "/app/main.py", Mode: 644},
	})
	t.check(err == nil, "upload /app/main.py: %v", err)

	// Streamed: each tick must arrive while the program is still running, not in one lump at
	// the end. The gap between the first and last event is the proof.
	var mu sync.Mutex
	var stamps []time.Time
	var lines []string

	ex, err := sb.RunCommand(ctx, "python /app/main.py", &opensandbox.ExecutionHandlers{
		OnStdout: func(m opensandbox.OutputMessage) error {
			mu.Lock()
			stamps = append(stamps, time.Now())
			lines = append(lines, m.Text)
			mu.Unlock()

			return nil
		},
	})
	t.must(err, "run main.py")

	joined := strings.Join(lines, "")
	t.check(strings.Contains(joined, "tick 0") && strings.Contains(joined, "tick 2"), "stdout carries every tick: %q", clip(joined))
	t.check(len(stamps) >= 2 && stamps[len(stamps)-1].Sub(stamps[0]) > 500*time.Millisecond,
		"output streamed as it was printed (first-to-last %v)", spread(stamps))
	t.check(ex.ExitCode != nil && *ex.ExitCode == 0, "exit code 0")

	_, _, code, err := run(ctx, sb, "python -c 'raise SystemExit(7)'")
	t.check(err == nil && code == 7, "a failing program's exit code comes back (got %d, %v)", code, err)

	// A persistent shell: an agent that cd's and exports expects the next command to see it.
	sess, err := sb.CreateSession(ctx)
	if t.check(err == nil && sess.ID != "", "a bash session is created") {
		_, err = sb.RunInSession(ctx, sess.ID, opensandbox.RunInSessionRequest{Command: "cd /app && export AGENT_STEP=two"}, nil)
		t.check(err == nil, "cd and export in the session")

		ex, err := sb.RunInSession(ctx, sess.ID, opensandbox.RunInSessionRequest{Command: "echo $PWD $AGENT_STEP"}, nil)
		t.check(err == nil && strings.Contains(ex.Text(), "/app two"), "the next command in the session sees both: %q", ex.Text())
		t.check(sb.DeleteSession(ctx, sess.ID) == nil, "delete the session")
	}

	x, _, _, err := execd(ctx, sb)
	t.must(err, "execd endpoint")

	bg, err := sb.RunCommandWithOpts(ctx, opensandbox.RunCommandRequest{
		Command:    "python -u -c 'import time\nfor i in range(10**6):\n    print(\"line\", i)\n    time.sleep(0.2)'",
		Background: true,
	}, nil)
	t.must(err, "background command")
	t.check(bg.ID != "", "background returns an id at once (%s)", bg.ID)

	var logs string
	got := waitFor(ctx, 20*time.Second, 300*time.Millisecond, func() bool {
		l, lerr := x.GetCommandLogs(ctx, bg.ID, nil)
		if lerr == nil {
			logs = l.Output
		}

		return strings.Contains(logs, "line 3")
	})
	t.check(got, "its logs are readable while it runs: %q", clip(logs))

	st, err := x.GetCommandStatus(ctx, bg.ID)
	t.check(err == nil && st.Running, "status says running")

	t.check(x.InterruptCommand(ctx, bg.ID) == nil, "interrupt")

	stopped := waitFor(ctx, 15*time.Second, 300*time.Millisecond, func() bool {
		st, err = x.GetCommandStatus(ctx, bg.ID)
		return err == nil && !st.Running
	})
	t.check(stopped, "the interrupted command stops")

	if stopped && st.ExitCode != nil {
		t.check(*st.ExitCode != 0, "and reports a non-zero exit (%d)", *st.ExitCode)
	} else if stopped {
		t.fail("an interrupted command has no exit code in its status")
	}

	ps, _, _, _ := run(ctx, sb, "ps -eo args 2>/dev/null || ls /proc/*/cmdline")
	_, _, pcode, _ := run(ctx, sb, `for p in /proc/[0-9]*; do tr '\0' ' ' < $p/cmdline 2>/dev/null; echo; done | grep -q 'range(10\*\*6)'`)
	t.check(pcode != 0, "and no process of it is left behind (%s)", clip(ps))
}

func spread(ts []time.Time) time.Duration {
	if len(ts) < 2 {
		return 0
	}

	return ts[len(ts)-1].Sub(ts[0]).Round(time.Millisecond)
}

// caseInterpreter drives OpenSandbox's code-interpreter image the way its SDK's
// CodeInterpreter is meant to be used: a context that keeps state across cells, rich output,
// and an interrupt that stops a runaway cell without losing the context.
func caseInterpreter(ctx context.Context, t *T, e *env) {
	timeout := 600
	ci, err := opensandbox.CreateCodeInterpreter(ctx, e.cfg, opensandbox.CodeInterpreterCreateOptions{
		TimeoutSeconds: &timeout, ReadyTimeout: 3 * time.Minute,
	})
	t.must(err, "CreateCodeInterpreter")

	defer kill(ci.Sandbox)

	t.check(ci.IsHealthy(ctx), "the interpreter runtime is healthy")

	cc, err := ci.CreateContext(ctx, opensandbox.CreateContextRequest{Language: "python"})
	t.must(err, "create a python context")

	_, err = ci.ExecuteInContext(ctx, cc.ID, "python", "x = 21\nimport math", nil)
	t.check(err == nil, "cell 1 defines x")

	ex, err := ci.ExecuteInContext(ctx, cc.ID, "python", "x * 2", nil)
	t.check(err == nil && resultText(ex) == "42", "cell 2 sees cell 1's x: %q (%v)", resultText(ex), err)

	ex, err = ci.ExecuteInContext(ctx, cc.ID, "python", "print('printed', x)", nil)
	t.check(err == nil && strings.Contains(ex.Text(), "printed 21"), "stdout of a cell: %q", ex.Text())

	ex, err = ci.ExecuteInContext(ctx, cc.ID, "python", "1/0", nil)
	t.check(err == nil && ex.Error != nil && ex.Error.Name == "ZeroDivisionError", "an exception comes back as an error, not a crash: %+v", ex.Error)

	// An image through display(), which is how a figure reaches a notebook. Built from bytes rather
	// than matplotlib: opensandbox/code-interpreter:latest does not ship matplotlib (measured:
	// ModuleNotFoundError), and what is under test is the display_data path, not a plotting library.
	plot := "import base64\nfrom IPython.display import Image, display\n" +
		"display(Image(data=base64.b64decode('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==')))"
	ex, err = ci.ExecuteInContext(ctx, cc.ID, "python", plot, nil)
	t.check(err == nil && hasMIME(ex, "image/png"), "an image comes back as an image/png result (%s, %v, error=%v, stdout=%q)", mimes(ex), err, execErr(ex), ex.Text())

	html := "from IPython.display import display, HTML\ndisplay(HTML('<b>hi</b>'))"
	ex, err = ci.ExecuteInContext(ctx, cc.ID, "python", html, nil)
	t.check(err == nil && hasMIME(ex, "text/html"), "display() output comes back as a result (%s)", mimes(ex))

	x, _, _, err := execd(ctx, ci.Sandbox)
	t.must(err, "execd endpoint")

	done := make(chan *opensandbox.Execution, 1)
	started := time.Now()

	go func() {
		ex, _ := ci.ExecuteInContext(ctx, cc.ID, "python", "import time\nwhile True:\n    time.sleep(0.05)", nil)
		done <- ex
	}()

	time.Sleep(2 * time.Second)
	t.check(x.InterruptCode(ctx, cc.ID) == nil, "interrupt the busy cell")

	select {
	case ex := <-done:
		t.check(ex != nil && ex.Error != nil && ex.Error.Name == "KeyboardInterrupt",
			"the busy cell ends with KeyboardInterrupt after %v", time.Since(started).Round(time.Millisecond))
	case <-time.After(30 * time.Second):
		t.fail("the busy cell was still running 30s after the interrupt")
	}

	ex, err = ci.ExecuteInContext(ctx, cc.ID, "python", "x + 1", nil)
	t.check(err == nil && resultText(ex) == "22", "the context survived the interrupt: %q", resultText(ex))
}

func resultText(ex *opensandbox.Execution) string {
	if ex == nil {
		return ""
	}

	// "text", not only Text() (which reads "text/plain"): upstream execd renames text/plain to
	// text on the way out (components/execd/pkg/web/controller/sse.go at the pinned commit), so
	// against upstream and sbx alike the pinned SDK's Text() is empty for an expression's value.
	for _, r := range ex.Results {
		for _, k := range []string{"text/plain", "text"} {
			if s := r.Results[k]; s != "" {
				return strings.TrimSpace(s)
			}
		}
	}

	return ""
}

func hasMIME(ex *opensandbox.Execution, mime string) bool {
	if ex == nil {
		return false
	}

	for _, r := range ex.Results {
		if r.Results[mime] != "" {
			return true
		}
	}

	return false
}

func mimes(ex *opensandbox.Execution) string {
	if ex == nil {
		return "no execution"
	}

	var out []string
	for _, r := range ex.Results {
		for k := range r.Results {
			out = append(out, k)
		}
	}

	return "[" + strings.Join(out, " ") + "]"
}

func execErr(ex *opensandbox.Execution) string {
	if ex == nil || ex.Error == nil {
		return "none"
	}

	return ex.Error.Name + ": " + clip(ex.Error.Value)
}
