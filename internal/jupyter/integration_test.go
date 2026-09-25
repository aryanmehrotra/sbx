package jupyter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealJupyter runs the engine against a real Jupyter Server: by default upstream's own
// opensandbox/code-interpreter image, the one its code-interpreter e2e uses, so kernel names,
// the token and the event stream are checked against the real thing rather than the fake's idea
// of it.
//
// That image starts Jupyter with --ip=127.0.0.1, so it is unreachable through a published port.
// Rather than patch the image, the test does what execd will do: it runs inside the container.
// With SBX_JUPYTER_IT=1 the host half starts one uniquely named container, cross-compiles this
// test binary for it, copies it in and runs it there with SBX_JUPYTER_URL set; that inner run is
// the one that talks to Jupyter. SBX_JUPYTER_URL can also be set directly to test any reachable
// server. SBX_JUPYTER_IMAGE and SBX_JUPYTER_TOKEN override the image and its token.
//
// Opt-in, because the image is 2.4 GiB compressed. Only the container it creates is removed.
func TestRealJupyter(t *testing.T) {
	if testing.Short() {
		t.Skip("real Jupyter test skipped in -short")
	}

	token := envOr("SBX_JUPYTER_TOKEN", "opensandboxcodeinterpreterjupyter") // the image's ENV JUPYTER_TOKEN

	base := os.Getenv("SBX_JUPYTER_URL")
	if base == "" {
		if os.Getenv("SBX_JUPYTER_IT") != "1" {
			t.Skip("set SBX_JUPYTER_IT=1 to run against a real Jupyter image (pulls opensandbox/code-interpreter)")
		}

		runInsideContainer(t, token)

		return
	}

	e := New(Config{BaseURL: base, Token: token, StartupWait: 90 * time.Second})

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	for deadline := time.Now().Add(90 * time.Second); ; {
		err := e.Available(ctx)
		if err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("jupyter never became available: %v", err)
		}

		time.Sleep(time.Second)
	}

	if ks, err := e.rest.kernelSpecs(ctx); err == nil {
		for n, s := range ks.Kernelspecs {
			t.Logf("kernelspec %-20s language=%s", n, s.Spec.Language)
		}
	}

	run := func(req RunRequest) []Event {
		t.Helper()

		var evs []Event

		if err := e.Run(ctx, req, func(ev Event) { evs = append(evs, ev) }); err != nil {
			t.Fatalf("Run(%q): %v", req.Code, err)
		}

		return evs
	}

	find := func(evs []Event, typ EventType) *Event {
		for i := range evs {
			if evs[i].Type == typ {
				return &evs[i]
			}
		}

		return nil
	}

	t.Run("bare python run: stdout, result, count", func(t *testing.T) {
		evs := run(RunRequest{Language: "python", Code: "print('hello from python')\n2 + 2"})
		t.Logf("events: %v", types(evs))

		if ev := find(evs, EventStdout); ev == nil || ev.Text != "hello from python\n" {
			t.Fatalf("stdout = %+v", ev)
		}

		if ev := find(evs, EventResult); ev == nil || ev.Results["text"] != "4" {
			t.Fatalf("result = %+v", ev)
		}

		if ev := find(evs, EventExecutionCount); ev == nil || ev.ExecutionCount < 1 {
			t.Fatalf("execution_count = %+v", ev)
		}

		if last := evs[len(evs)-1]; last.Type != EventComplete {
			t.Fatalf("last event = %+v", last)
		}
	})

	t.Run("exception", func(t *testing.T) {
		evs := run(RunRequest{Language: "python", Code: "1/0"})

		ev := find(evs, EventError)
		if ev == nil || ev.Error.EName != "ZeroDivisionError" || len(ev.Error.Traceback) == 0 {
			t.Fatalf("error = %+v", ev)
		}

		if got := types(evs); got[len(got)-1] != EventComplete {
			t.Fatalf("events = %v", got)
		}
	})

	t.Run("context state and isolation", func(t *testing.T) {
		// Relative: Jupyter resolves a session path under its root directory, so that is where a cwd
		// can point (an absolute path outside it falls back to the root, upstream included).
		// The test runs with the container WorkingDir, which is the server root.
		a, err := e.CreateContext(ctx, "python", "sbx-it")
		if err != nil {
			t.Fatal(err)
		}

		b, err := e.CreateContext(ctx, "python", "")
		if err != nil {
			t.Fatal(err)
		}

		run(RunRequest{ContextID: a.ID, Code: "x = 42"})

		if ev := find(run(RunRequest{ContextID: a.ID, Code: "import os; print(x, os.getcwd())"}), EventStdout); ev == nil || ev.Text != "42 /workspace/sbx-it\n" {
			t.Fatalf("context a = %+v", ev)
		}

		if ev := find(run(RunRequest{ContextID: b.ID, Code: "print('x' in dir())"}), EventStdout); ev == nil || ev.Text != "False\n" {
			t.Fatalf("context b sees a's state: %+v", ev)
		}
	})

	t.Run("interrupt", func(t *testing.T) {
		c, err := e.CreateContext(ctx, "python", "")
		if err != nil {
			t.Fatal(err)
		}

		done := make(chan []Event, 1)
		sleeping := make(chan struct{})

		go func() {
			var evs []Event

			_ = e.Run(ctx, RunRequest{ContextID: c.ID, Code: "import time\nprint('sleeping', flush=True)\ntime.sleep(60)"}, func(ev Event) {
				evs = append(evs, ev)
				if ev.Type == EventStdout {
					close(sleeping)
				}
			})
			done <- evs
		}()

		// Interrupt only once the cell is running: a fresh kernel takes seconds to start, and an
		// interrupt that lands before the cell does interrupts nothing.
		select {
		case <-sleeping:
		case <-time.After(60 * time.Second):
			t.Fatal("the cell never started")
		}

		if err := e.Interrupt(ctx, c.ID); err != nil {
			t.Fatal(err)
		}

		select {
		case evs := <-done:
			if ev := find(evs, EventError); ev == nil || ev.Error.EName != "KeyboardInterrupt" {
				t.Fatalf("events = %v, error = %+v", types(evs), ev)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("interrupt did not end the cell")
		}
	})

	t.Run("other kernels", func(t *testing.T) {
		for lang, code := range map[string]string{"bash": "echo hi-from-bash", "javascript": "console.log('hi-from-js')"} {
			evs := run(RunRequest{Language: lang, Code: code})
			if ev := find(evs, EventStdout); ev == nil || !strings.Contains(ev.Text, "hi-from-") {
				t.Errorf("%s: events = %v", lang, types(evs))
			}
		}
	})

	t.Run("delete by language", func(t *testing.T) {
		if err := e.DeleteContextsByLanguage(ctx, "python"); err != nil {
			t.Fatal(err)
		}

		if l := e.ListContexts("python"); len(l) != 0 {
			t.Fatalf("left: %v", l)
		}
	})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}

	return def
}

// runInsideContainer is the host half of TestRealJupyter.
func runInsideContainer(t *testing.T, token string) {
	image := envOr("SBX_JUPYTER_IMAGE", "opensandbox/code-interpreter:latest")
	name := "sbx-jupyter-it-" + newID()[:12]

	arch, err := exec.Command("docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("docker version: %v - is docker running?", err)
	}

	bin := filepath.Join(t.TempDir(), "jupyter.test")

	build := exec.Command("go", "test", "-c", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0")

	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cross-compile the test binary: %v\n%s", err, out)
	}

	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name, image).CombinedOutput(); err != nil {
		t.Fatalf("docker run %s: %v\n%s", image, err, out)
	}

	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := exec.Command("docker", "exec", name, "tail", "-40", "/opt/code-interpreter/jupyter.log").CombinedOutput()
			t.Logf("jupyter.log:\n%s", logs)
		}

		_ = exec.Command("docker", "rm", "-f", name).Run()
	})

	if out, err := exec.Command("docker", "cp", bin, name+":/tmp/jupyter.test").CombinedOutput(); err != nil {
		t.Fatalf("docker cp: %v\n%s", err, out)
	}

	out, err := exec.Command("docker", "exec", "-e", "SBX_JUPYTER_URL=http://127.0.0.1:44771", "-e", "SBX_JUPYTER_TOKEN="+token,
		name, "/tmp/jupyter.test", "-test.run", "^TestRealJupyter$", "-test.v", "-test.timeout", "5m").CombinedOutput()
	t.Logf("inside the container:\n%s", out)

	if err != nil {
		t.Fatalf("the in-container run failed: %v", err)
	}
}
