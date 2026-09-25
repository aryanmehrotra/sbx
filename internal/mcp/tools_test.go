package mcp

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

// upstreamTools is the tool surface of upstream's MCP server at release-1.1.0
// (sdks/mcp/sandbox/python/src/opensandbox_mcp/server.py). sbx serves exactly these names, so
// an agent configured for one works with the other.
var upstreamTools = []string{
	"command_interrupt", "command_run",
	"file_create_directories", "file_delete", "file_delete_directories", "file_move", "file_read",
	"file_replace_contents", "file_search", "file_write",
	"sandbox_connect", "sandbox_create", "sandbox_get_endpoint", "sandbox_get_info", "sandbox_get_metrics",
	"sandbox_healthcheck", "sandbox_kill", "sandbox_list", "sandbox_renew",
}

func TestTheToolSurfaceIsUpstreams(t *testing.T) {
	_, c := sandboxServer(t)

	tools, _ := obj(c.call("tools/list", nil)["result"])["tools"].([]any)

	var names []string

	for _, x := range tools {
		tl := obj(x)
		names = append(names, tl["name"].(string))

		schema := obj(tl["inputSchema"])
		props := obj(schema["properties"])

		// Every required argument must be a declared property; a schema that requires what
		// it never describes is one no client can satisfy.
		req, _ := schema["required"].([]any)
		for _, r := range req {
			if props[r.(string)] == nil {
				t.Errorf("%s requires %q but does not declare it", tl["name"], r)
			}
		}

		if tl["description"] == "" {
			t.Errorf("%s has no description", tl["name"])
		}
	}

	slices.Sort(names)

	if !slices.Equal(names, upstreamTools) {
		t.Fatalf("tools = %v\nwant upstream's %v", names, upstreamTools)
	}
}

// requiredOf returns a tool's required arguments, to check them against upstream's
// signatures (a parameter without a default is required there).
func requiredOf(t *testing.T, c *client, name string) []string {
	t.Helper()

	tools, _ := obj(c.call("tools/list", nil)["result"])["tools"].([]any)
	for _, x := range tools {
		if tl := obj(x); tl["name"] == name {
			var out []string

			req, _ := obj(tl["inputSchema"])["required"].([]any)
			for _, r := range req {
				out = append(out, r.(string))
			}

			slices.Sort(out)

			return out
		}
	}

	t.Fatalf("no tool %s", name)

	return nil
}

func TestRequiredArgumentsMatchUpstreamSignatures(t *testing.T) {
	_, c := sandboxServer(t)

	for name, want := range map[string][]string{
		"sandbox_create":          {"image"},
		"sandbox_connect":         {"sandbox_id"},
		"sandbox_renew":           {"sandbox_id", "timeout_seconds"},
		"sandbox_list":            nil,
		"sandbox_get_endpoint":    {"port", "sandbox_id"},
		"command_run":             {"command", "sandbox_id"},
		"command_interrupt":       {"execution_id", "sandbox_id"},
		"file_read":               {"path", "sandbox_id"},
		"file_write":              {"content", "path", "sandbox_id"},
		"file_search":             {"path", "pattern", "sandbox_id"},
		"file_move":               {"entries", "sandbox_id"},
		"file_replace_contents":   {"entries", "sandbox_id"},
		"file_create_directories": {"entries", "sandbox_id"},
	} {
		if got := requiredOf(t, c, name); !slices.Equal(got, want) {
			t.Errorf("%s requires %v, upstream requires %v", name, got, want)
		}
	}
}

func TestSandboxCreateWaitsAndReturnsInfo(t *testing.T) {
	f, c := sandboxServer(t)
	f.PendingPolls = 2

	out := obj(c.ok("sandbox_create", map[string]any{
		"image":         "python:3.12",
		"auth_username": "u", "auth_password": "p",
		"metadata":       map[string]string{"task": "t1"},
		"env":            map[string]string{"A": "1"},
		"network_policy": map[string]any{"defaultAction": "deny", "egress": []any{map[string]any{"action": "allow", "target": "pypi.org"}}},
	}))

	id, _ := out["sandbox_id"].(string)
	info := obj(out["info"])

	must(t, id != "" && info["id"] == id, "create result = %v", out)
	must(t, obj(info["status"])["state"] == "Running", "state after waiting = %v", info["status"])
	must(t, obj(info["image"])["image"] == "python:3.12", "info.image = %v", info["image"])
	must(t, info["expires_at"] != nil && info["created_at"] != nil, "timestamps missing: %v", info)
	must(t, obj(info["metadata"])["task"] == "t1", "metadata = %v", info["metadata"])

	// What went over the wire: upstream SDK defaults filled in, camelCase, credentials sent.
	req, _ := f.Last("POST", "/v1/sandboxes")

	var body map[string]any

	_ = json.Unmarshal(req.Body, &body)
	must(t, body["timeout"] == 600.0, "timeout = %v, want upstream's 600 default", body["timeout"])
	must(t, obj(body["resourceLimits"])["memory"] == "2Gi", "resourceLimits = %v", body["resourceLimits"])
	must(t, len(body["entrypoint"].([]any)) == 3, "entrypoint = %v", body["entrypoint"])
	must(t, obj(obj(body["image"])["auth"])["username"] == "u", "image auth = %v", body["image"])
	must(t, obj(body["networkPolicy"])["defaultAction"] == "deny", "networkPolicy = %v", body["networkPolicy"])

	// The readiness wait happened: execd was pinged.
	_, pinged := f.Last("GET", "/ping")
	must(t, pinged, "create returned without pinging execd")
}

func TestSandboxCreateThatNeverBecomesReadyIsRemoved(t *testing.T) {
	f, c := sandboxServer(t)
	f.PendingPolls = 1 << 30

	msg := c.fails("sandbox_create", map[string]any{"image": "alpine", "ready_timeout_seconds": 0.2, "health_check_polling_interval_ms": 10})

	must(t, strings.Contains(msg, "removed") && strings.Contains(msg, "ready_timeout_seconds"),
		"error does not say what happened and what to do: %q", msg)

	list := obj(c.ok("sandbox_list", map[string]any{}))
	must(t, len(list["sandbox_infos"].([]any)) == 0, "the unready sandbox was left behind: %v", list)
}

func TestSandboxCreateRejectsBadArguments(t *testing.T) {
	_, c := sandboxServer(t)

	c.fails("sandbox_create", map[string]any{"image": ""})
	msg := c.fails("sandbox_create", map[string]any{"image": "alpine", "auth_username": "u"})
	must(t, strings.Contains(msg, "together"), "half the credentials: %q", msg)

	// A server-side refusal (timeout under 60) comes back as a tool error with its message.
	msg = c.fails("sandbox_create", map[string]any{"image": "alpine", "timeout_seconds": 5})
	must(t, strings.Contains(msg, "at least 60"), "server refusal lost its message: %q", msg)
}

func TestLifecycleTools(t *testing.T) {
	f, c := sandboxServer(t)
	id := f.AddSandbox("alpine")

	out := obj(c.ok("sandbox_connect", map[string]any{"sandbox_id": id}))
	must(t, out["sandbox_id"] == id, "connect = %v", out)

	info := obj(c.ok("sandbox_get_info", map[string]any{"sandbox_id": id}))
	must(t, info["id"] == id && obj(info["status"])["state"] == "Running", "get_info = %v", info)

	h := obj(c.ok("sandbox_healthcheck", map[string]any{"sandbox_id": id}))
	must(t, h["healthy"] == true && h["sandbox_id"] == id, "healthcheck = %v", h)

	m := obj(c.ok("sandbox_get_metrics", map[string]any{"sandbox_id": id}))
	must(t, m["cpu_count"] == 2.0 && m["memory_used_in_mib"] == 256.0 && m["cpu_used_percentage"] == 12.5,
		"metrics not in upstream's field names: %v", m)

	ep := obj(c.ok("sandbox_get_endpoint", map[string]any{"sandbox_id": id, "port": 8000}))
	must(t, strings.HasSuffix(ep["endpoint"].(string), "/proxy/8000") && ep["headers"] != nil, "endpoint = %v", ep)

	before := time.Now()
	r := obj(c.ok("sandbox_renew", map[string]any{"sandbox_id": id, "timeout_seconds": 3600}))

	at, err := time.Parse(time.RFC3339Nano, r["expires_at"].(string))
	must(t, err == nil && at.Sub(before) > 59*time.Minute && at.Sub(before) < 61*time.Minute,
		"renew is from now: %v (%v)", r, err)

	c.fails("sandbox_renew", map[string]any{"sandbox_id": id, "timeout_seconds": 0})

	k := obj(c.ok("sandbox_kill", map[string]any{"sandbox_id": id}))
	must(t, k["status"] == "killed" && !f.Exists(id), "kill = %v, exists %v", k, f.Exists(id))

	msg := c.fails("sandbox_get_info", map[string]any{"sandbox_id": id})
	must(t, strings.Contains(msg, "404") && strings.Contains(msg, "sandbox_list"), "missing sandbox: %q", msg)

	msg = c.fails("sandbox_healthcheck", map[string]any{"sandbox_id": id})
	must(t, strings.Contains(msg, "404"), "healthcheck of a missing sandbox should fail, not say unhealthy: %q", msg)
}

func TestSandboxList(t *testing.T) {
	f, c := sandboxServer(t)
	f.AddSandbox("alpine")
	f.AddSandbox("alpine")

	out := obj(c.ok("sandbox_list", map[string]any{"filter": map[string]any{"states": []string{"Running"}, "page_size": 1}}))
	p := obj(out["pagination"])

	must(t, len(out["sandbox_infos"].([]any)) == 1, "page of 1 = %v", out)
	must(t, p["page_size"] == 1.0 && p["total_items"] == 2.0 && p["has_next_page"] == true,
		"pagination not in upstream's field names: %v", p)

	c.fails("sandbox_list", map[string]any{"filter": map[string]any{"page_size": 0}})
}

func TestCommandRunAndInterrupt(t *testing.T) {
	f, c := sandboxServer(t)
	id := f.AddSandbox("alpine")

	// Resolved on demand: no sandbox_connect first, unlike upstream's registry.
	x := obj(c.ok("command_run", map[string]any{"sandbox_id": id, "command": "echo hi; warn w; pwd", "working_directory": "/app"}))

	logs := obj(x["logs"])
	stdout := logs["stdout"].([]any)

	must(t, x["exit_code"] == 0.0 && x["id"] != nil && x["complete"] != nil, "execution = %v", x)
	must(t, len(stdout) == 2 && obj(stdout[0])["text"] == "hi" && obj(stdout[1])["text"] == "/app", "stdout = %v", stdout)
	must(t, obj(logs["stderr"].([]any)[0])["is_error"] == true, "stderr = %v", logs["stderr"])

	for _, k := range []string{"id", "execution_count", "result", "error", "complete", "exit_code", "logs"} {
		if _, ok := x[k]; !ok {
			t.Errorf("execution lacks upstream's %q field: %v", k, x)
		}
	}

	x = obj(c.ok("command_run", map[string]any{"sandbox_id": id, "command": "exit 2"}))
	must(t, x["exit_code"] == 2.0 && obj(x["error"])["value"] == "2", "a failing command is a result with its exit code, not a tool error: %v", x)

	start := time.Now()
	x = obj(c.ok("command_run", map[string]any{"sandbox_id": id, "command": "sleep", "background": true}))
	must(t, time.Since(start) < 2*time.Second && x["id"] != nil && x["exit_code"] == nil, "background = %v", x)

	// Interrupt a foreground command from a second call while the first is running.
	runID := c.request("tools/call", map[string]any{"name": "command_run", "arguments": map[string]any{"sandbox_id": id, "command": "sleep"}})

	deadline := time.Now().Add(5 * time.Second)
	for f.Running() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	ids := f.RunningIDs()
	if len(ids) != 1 {
		t.Fatalf("running commands = %v, want the one just started", ids)
	}

	execID := ids[0]

	st := obj(c.ok("command_interrupt", map[string]any{"sandbox_id": id, "execution_id": execID}))
	must(t, st["status"] == "interrupted", "interrupt = %v", st)

	res := obj(c.await(runID)["result"])
	var ex map[string]any

	_ = json.Unmarshal([]byte(text(res)), &ex)
	must(t, ex["exit_code"] == 130.0, "interrupted run = %v", ex)

	c.fails("command_interrupt", map[string]any{"sandbox_id": id, "execution_id": "cmd-nope"})
	c.fails("command_run", map[string]any{"sandbox_id": id, "command": "  "})
}

func TestCancellingCommandRunInterruptsIt(t *testing.T) {
	f, c := sandboxServer(t)
	id := f.AddSandbox("alpine")

	runID := c.request("tools/call", map[string]any{"name": "command_run", "arguments": map[string]any{"sandbox_id": id, "command": "sleep"}})

	deadline := time.Now().Add(5 * time.Second)
	for f.Running() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": runID}})

	for f.Running() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	must(t, f.Running() == 0, "cancelling the call left the command running in the sandbox")

	_, interrupted := f.Last("DELETE", "/command")
	must(t, interrupted, "cancelling the call did not send an interrupt")
}

func TestFileTools(t *testing.T) {
	f, c := sandboxServer(t)
	id := f.AddSandbox("alpine")

	st := obj(c.ok("file_write", map[string]any{"sandbox_id": id, "path": "/app/a.py", "content": "host = 'localhost'\n", "mode": 644}))
	must(t, st["status"] == "written", "write = %v", st)

	got, _ := f.File(id, "/app/a.py")
	must(t, got == "host = 'localhost'\n", "file content = %q", got)

	up, _ := f.Last("POST", "/files/upload")
	must(t, strings.Contains(string(up.Body), `"mode":644`), "mode not sent as upstream's octal digits: %s", up.Body)

	r := obj(c.ok("file_read", map[string]any{"sandbox_id": id, "path": "/app/a.py"}))
	must(t, r["content"] == "host = 'localhost'\n" && r["path"] == "/app/a.py", "read = %v", r)

	r = obj(c.ok("file_read", map[string]any{"sandbox_id": id, "path": "/app/a.py", "range_header": "bytes=0-3"}))
	must(t, r["content"] == "host", "range read = %v", r)

	f.PutFile(id, "/app/bin", "\xff\xfe")
	msg := c.fails("file_read", map[string]any{"sandbox_id": id, "path": "/app/bin"})
	must(t, strings.Contains(msg, "latin-1"), "binary read should suggest what to do: %q", msg)

	r = obj(c.ok("file_read", map[string]any{"sandbox_id": id, "path": "/app/bin", "encoding": "latin-1"}))
	must(t, r["content"] == "ÿþ", "latin-1 read = %q", r["content"])

	c.fails("file_read", map[string]any{"sandbox_id": id, "path": "/app/a.py", "encoding": "utf-16"})
	c.fails("file_read", map[string]any{"sandbox_id": id, "path": "/nope"})

	rep := c.ok("file_replace_contents", map[string]any{"sandbox_id": id, "entries": []any{
		map[string]any{"path": "/app/a.py", "old_content": "localhost", "new_content": "0.0.0.0"},
	}}).([]any)
	must(t, len(rep) == 1 && obj(rep[0])["replaced_count"] == 1.0 && obj(rep[0])["path"] == "/app/a.py", "replace = %v", rep)

	got, _ = f.File(id, "/app/a.py")
	must(t, got == "host = '0.0.0.0'\n", "after replace = %q", got)

	found := c.ok("file_search", map[string]any{"sandbox_id": id, "path": "/app", "pattern": "*.py"}).([]any)
	must(t, len(found) == 1 && obj(found[0])["path"] == "/app/a.py" && obj(found[0])["type"] == "file" &&
		obj(found[0])["modified_at"] != nil, "search = %v", found)

	res := c.tool("file_search", map[string]any{"sandbox_id": id, "path": "/app", "pattern": "*.py"})
	must(t, obj(res["structuredContent"])["result"] != nil, "a list result is not wrapped for structuredContent: %v", res)

	mv := obj(c.ok("file_move", map[string]any{"sandbox_id": id, "entries": []any{
		map[string]any{"source": "/app/a.py", "destination": "/app/b.py"},
	}}))
	_, moved := f.File(id, "/app/b.py")
	must(t, mv["status"] == "moved" && moved, "move = %v", mv)

	// upstream's model also populates by field name
	c.ok("file_move", map[string]any{"sandbox_id": id, "entries": []any{map[string]any{"src": "/app/b.py", "dest": "/app/c.py"}}})
	c.fails("file_move", map[string]any{"sandbox_id": id, "entries": []any{map[string]any{"source": "/app/c.py"}}})

	d := obj(c.ok("file_delete", map[string]any{"sandbox_id": id, "paths": []string{"/app/c.py"}}))
	_, still := f.File(id, "/app/c.py")
	must(t, d["status"] == "deleted" && !still, "delete = %v", d)

	c.fails("file_delete", map[string]any{"sandbox_id": id, "paths": []string{}})

	mk := obj(c.ok("file_create_directories", map[string]any{"sandbox_id": id, "entries": []any{
		map[string]any{"path": "/work/x"}, map[string]any{"path": "/work/y", "mode": 700},
	}}))
	mx, okx := f.Dir(id, "/work/x")
	my, _ := f.Dir(id, "/work/y")
	must(t, mk["status"] == "created" && okx && mx == 755 && my == 700, "mkdir = %v modes %d %d", mk, mx, my)

	rm := obj(c.ok("file_delete_directories", map[string]any{"sandbox_id": id, "paths": []string{"/work/x"}}))
	_, gone := f.Dir(id, "/work/x")
	must(t, rm["status"] == "deleted" && !gone, "rmdir = %v", rm)
}

func TestUnreachableServerSaysWhatToDo(t *testing.T) {
	f, c := sandboxServer(t)
	f.Close()

	msg := c.fails("sandbox_list", map[string]any{})
	must(t, strings.Contains(msg, "sbx serve --osb-addr") && strings.Contains(msg, "SBX_OSB_URL"),
		"unreachable server: %q", msg)
}
