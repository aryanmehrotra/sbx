package app

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/osbclient/osbtest"
)

func TestOSBTargetPrecedence(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	cases := []struct {
		name             string
		flagURL, flagKey string
		env              map[string]string
		url, key         string
	}{
		{"nothing set", "", "", nil, defaultOSBURL, ""},
		{"upstream's names are read", "", "", map[string]string{
			"OPEN_SANDBOX_DOMAIN": "osb.example.com", "OPEN_SANDBOX_API_KEY": "up",
		}, "osb.example.com", "up"},
		{"sbx's names win over upstream's", "", "", map[string]string{
			"OPEN_SANDBOX_DOMAIN": "osb.example.com", "OPEN_SANDBOX_API_KEY": "up",
			"SBX_OSB_URL": "http://127.0.0.1:9", "SBX_OSB_KEY": "mine",
		}, "http://127.0.0.1:9", "mine"},
		{"flags win over everything", "http://h:1", "f", map[string]string{
			"SBX_OSB_URL": "http://127.0.0.1:9", "SBX_OSB_KEY": "mine",
		}, "http://h:1", "f"},
	}

	for _, c := range cases {
		url, key := osbTarget(c.flagURL, c.flagKey, env(c.env))
		if url != c.url || key != c.key {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, url, key, c.url, c.key)
		}
	}
}

// buildSbx compiles the real binary once per test run.
func buildSbx(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH to build the binary with")
	}

	bin := filepath.Join(t.TempDir(), "sbx")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}

	cmd := exec.Command("go", "build", "-o", bin, "github.com/aryanmehrotra/sbx")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building sbx: %v\n%s", err, out)
	}

	return bin
}

// mcpProc is `sbx mcp` running as a subprocess, spoken to over its real stdin and stdout.
type mcpProc struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	stderr *strings.Builder
	id     int
}

func startMCP(t *testing.T, bin string, env []string, args ...string) *mcpProc {
	t.Helper()

	home := t.TempDir()
	cmd := exec.Command(bin, append([]string{"mcp"}, args...)...)
	// A home of its own, so the run cannot touch the real ~/.sbx or its history file.
	cmd.Env = append(os.Environ(), append([]string{
		"HOME=" + home, "USERPROFILE=" + home, "SBX_HISTORY=" + filepath.Join(home, "history.jsonl"),
		"SBX_OSB_URL=", "SBX_OSB_KEY=", "OPEN_SANDBOX_DOMAIN=", "OPEN_SANDBOX_API_KEY=",
	}, env...)...)

	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()

	var stderr strings.Builder

	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	p := &mcpProc{t: t, cmd: cmd, in: in, out: bufio.NewReader(out), stderr: &stderr}

	t.Cleanup(func() {
		if p.cmd.ProcessState == nil {
			_ = p.cmd.Process.Kill()
			_ = p.cmd.Wait()
		}
	})

	return p
}

// call sends a request and reads lines until its answer, failing on any line that is not a
// JSON-RPC message - which is the whole contract of stdout here.
func (p *mcpProc) call(method string, params any) map[string]any {
	p.t.Helper()

	p.id++
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": p.id, "method": method, "params": params})

	if _, err := p.in.Write(append(msg, '\n')); err != nil {
		p.t.Fatalf("writing to sbx mcp: %v (stderr: %s)", err, p.stderr)
	}

	type line struct {
		s   string
		err error
	}

	for {
		ch := make(chan line, 1)

		go func() {
			s, err := p.out.ReadString('\n')
			ch <- line{s, err}
		}()

		var l line

		select {
		case l = <-ch:
		case <-time.After(15 * time.Second):
			p.t.Fatalf("no answer to %s (stderr: %s)", method, p.stderr)
		}

		if l.err != nil {
			p.t.Fatalf("sbx mcp closed stdout: %v (stderr: %s)", l.err, p.stderr)
		}

		var m map[string]any
		if err := json.Unmarshal([]byte(l.s), &m); err != nil || m["jsonrpc"] != "2.0" {
			p.t.Fatalf("stdout carried a line that is not JSON-RPC: %q", l.s)
		}

		if n, ok := m["id"].(float64); ok && int(n) == p.id {
			return m
		}
	}
}

func (p *mcpProc) tool(name string, args any) map[string]any {
	p.t.Helper()

	r := p.call("tools/call", map[string]any{"name": name, "arguments": args})

	res, _ := r["result"].(map[string]any)
	if res == nil || res["isError"] == true {
		p.t.Fatalf("%s: %v", name, r)
	}

	sc, _ := res["structuredContent"].(map[string]any)

	return sc
}

// finish closes stdin and requires a clean exit.
func (p *mcpProc) finish() {
	p.t.Helper()

	_ = p.in.Close()

	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			p.t.Fatalf("sbx mcp exited with %v after stdin closed (stderr: %s)", err, p.stderr)
		}
	case <-time.After(10 * time.Second):
		p.t.Fatal("sbx mcp did not exit after its stdin closed")
	}
}

// TestMCPEndToEnd runs the real binary as an MCP client would: a subprocess, spoken to over its
// own stdin and stdout, against a fake OpenSandbox server.
func TestMCPEndToEnd(t *testing.T) {
	bin := buildSbx(t)

	t.Run("stdio round trip", func(t *testing.T) { mcpRoundTrip(t, bin) })
	t.Run("upstream's environment", func(t *testing.T) { mcpUpstreamEnv(t, bin) })
}

func mcpRoundTrip(t *testing.T, bin string) {

	f := osbtest.New()
	f.Key = "e2e-key"
	defer f.Close()

	p := startMCP(t, bin, []string{"SBX_OSB_KEY=e2e-key"}, "--url", f.URL)

	init := p.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"},
	})

	res, _ := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize = %v", init)
	}

	if _, err := p.in.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")); err != nil {
		t.Fatal(err)
	}

	tools, _ := p.call("tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 19 {
		t.Fatalf("tools/list has %d tools, want upstream's 19", len(tools))
	}

	created := p.tool("sandbox_create", map[string]any{"image": "alpine"})
	id, _ := created["sandbox_id"].(string)

	if id == "" {
		t.Fatalf("sandbox_create = %v", created)
	}

	p.tool("file_write", map[string]any{"sandbox_id": id, "path": "/app/x.txt", "content": "hello"})

	if got := p.tool("file_read", map[string]any{"sandbox_id": id, "path": "/app/x.txt"}); got["content"] != "hello" {
		t.Fatalf("file_read = %v", got)
	}

	ran := p.tool("command_run", map[string]any{"sandbox_id": id, "command": "echo from-e2e"})

	stdout, _ := ran["logs"].(map[string]any)["stdout"].([]any)
	if len(stdout) != 1 || stdout[0].(map[string]any)["text"] != "from-e2e" || ran["exit_code"] != 0.0 {
		t.Fatalf("command_run = %v", ran)
	}

	if k := p.tool("sandbox_kill", map[string]any{"sandbox_id": id}); k["status"] != "killed" || f.Exists(id) {
		t.Fatalf("sandbox_kill = %v", k)
	}

	p.finish()

	if !strings.Contains(p.stderr.String(), "serving MCP on stdio") {
		t.Errorf("stderr = %q, want the startup line there and not on stdout", p.stderr)
	}
}

func mcpUpstreamEnv(t *testing.T, bin string) {

	f := osbtest.New()
	f.Key = "upstream-key"
	defer f.Close()

	// No flags: OPEN_SANDBOX_DOMAIN given as a bare host, as upstream's docs show it.
	p := startMCP(t, bin, []string{
		"OPEN_SANDBOX_DOMAIN=" + strings.TrimPrefix(f.URL, "http://"), "OPEN_SANDBOX_API_KEY=upstream-key",
	})

	p.call("initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "e2e", "version": "0"}})

	r := p.call("tools/call", map[string]any{"name": "sandbox_list", "arguments": map[string]any{}})

	res, _ := r["result"].(map[string]any)
	if res["isError"] != false {
		t.Fatalf("sandbox_list with upstream's env = %v (stderr: %s)", r, p.stderr)
	}

	p.finish()
}

func TestMCPRejectsArguments(t *testing.T) {
	err := runMCP([]string{"extra"})
	if err == nil || !strings.Contains(err.Error(), "no arguments") {
		t.Fatalf("runMCP(extra) = %v", err)
	}
}

// With no flag or variable, `sbx mcp` reads the key `sbx serve` generated - but only for a
// loopback server: that key must never be sent to somebody else's OpenSandbox.
func TestLocalKeyIsReadOnlyForALoopbackServer(t *testing.T) {
	read := func() string { return "from-file" }

	for url, want := range map[string]string{
		"http://127.0.0.1:8080":   "from-file",
		"http://localhost:8080":   "from-file",
		"http://[::1]:8080":       "from-file",
		"localhost:8080":          "from-file",
		"https://osb.example.com": "",
		"osb.example.com":         "",
		"http://10.0.0.5:8080":    "",
	} {
		if got := withLocalKey(url, "", read); got != want {
			t.Errorf("withLocalKey(%q) = %q, want %q", url, got, want)
		}
	}

	if got := withLocalKey("http://127.0.0.1:8080", "given", read); got != "given" {
		t.Errorf("a key already given was replaced by the file: %q", got)
	}
}
