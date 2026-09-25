//go:build unix

package execd

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Arguments travel in one variable split on \x1f (unit separator): an environment variable
// cannot hold NUL.
//
// The tests in this file run execd in a child process - a copy of this test binary with
// SBX_EXECD_TEST_MAIN set - because what they test is process-wide: signal handlers, the exit
// status, and a reaper that calls wait4(-1) and would otherwise reap other tests' children.
func TestMain(m *testing.M) {
	switch os.Getenv("SBX_EXECD_TEST_MAIN") {
	case "run":
		os.Exit(run(strings.Split(os.Getenv("SBX_EXECD_TEST_ARGS"), "\x1f"), os.Stderr))
	case "reaper":
		os.Exit(reaperScenario())
	}

	os.Exit(m.Run())
}

func freeAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := ln.Addr().String()
	_ = ln.Close()

	return addr
}

// startExecd runs `sbx execd args...` as a child and returns it with its stderr.
func startExecd(t *testing.T, env []string, args ...string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "SBX_EXECD_TEST_MAIN=run", "SBX_EXECD_TEST_ARGS="+strings.Join(args, "\x1f"))
	cmd.Env = append(cmd.Env, env...)

	var stderr bytes.Buffer

	cmd.Stderr = &stderr
	// A child execd leaves behind when a test fails still holds the stderr pipe, and Wait would
	// block on it forever instead of letting the failure be reported.
	cmd.WaitDelay = 2 * time.Second

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	return cmd, &stderr
}

func waitExit(t *testing.T, cmd *exec.Cmd, within time.Duration) int {
	t.Helper()

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}

		if err != nil {
			t.Fatal(err)
		}

		return 0
	case <-time.After(within):
		t.Fatalf("execd did not exit within %s", within)
		return -1
	}
}

func waitServing(t *testing.T, addr string) {
	t.Helper()

	eventually(t, 10*time.Second, "execd answering /ping on "+addr, func() bool {
		resp, err := http.Get("http://" + addr + "/ping")
		if err != nil {
			return false
		}

		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	})
}

func TestExecdExitsWithItsChildsCode(t *testing.T) {
	addr := freeAddr(t)
	cmd, stderr := startExecd(t, []string{EnvGraceShutdown + "=0s"}, "--addr", addr, "--", "sh", "-c", "sleep 0.5; exit 7")

	if code := waitExit(t, cmd, 20*time.Second); code != 7 {
		t.Fatalf("exit %d, want the child's 7; stderr:\n%s", code, stderr)
	}
}

func TestExecdServesWhileTheChildRunsAndStopsAfter(t *testing.T) {
	addr := freeAddr(t)
	marker := filepath.Join(t.TempDir(), "stop")
	cmd, stderr := startExecd(t, []string{EnvGraceShutdown + "=50ms"},
		"--addr", addr, "--", "sh", "-c", "while [ ! -e "+marker+" ]; do sleep 0.05; done")

	waitServing(t, addr)

	// The API runs commands while the entrypoint is up...
	resp, err := http.Post("http://"+addr+"/command", "application/json", strings.NewReader(`{"command":"touch `+marker+`"}`))
	if err != nil {
		t.Fatal(err)
	}

	exec, err := parseExecution(resp.Body)
	_ = resp.Body.Close()

	if err != nil || exec.ExitCode == nil || *exec.ExitCode != 0 {
		t.Fatalf("command through the API: %+v %v", exec, err)
	}

	// ...and that command ended the entrypoint, so execd exits 0 with it.
	if code := waitExit(t, cmd, 20*time.Second); code != 0 {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
}

func TestExecdForwardsSignalsToItsChild(t *testing.T) {
	addr := freeAddr(t)
	ready := filepath.Join(t.TempDir(), "ready")
	cmd, stderr := startExecd(t, []string{EnvGraceShutdown + "=0s"}, "--addr", addr, "--", "sh", "-c",
		"trap 'exit 42' TERM; touch "+ready+"; while :; do sleep 0.05; done")

	waitServing(t, addr)
	eventually(t, 10*time.Second, "the child's trap", func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	if code := waitExit(t, cmd, 20*time.Second); code != 42 {
		t.Fatalf("exit %d, want 42 from the child's TERM trap; stderr:\n%s", code, stderr)
	}
}

func TestExecdWithoutAChildServesUntilSIGTERM(t *testing.T) {
	addr := freeAddr(t)
	cmd, stderr := startExecd(t, []string{EnvAccessToken + "=tok"}, "--addr", addr)

	waitServing(t, addr)

	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the token from %s is not enforced: %d", EnvAccessToken, resp.StatusCode)
	}

	_ = cmd.Process.Signal(syscall.SIGTERM)

	if code := waitExit(t, cmd, 20*time.Second); code != 0 {
		t.Fatalf("exit %d; stderr:\n%s", code, stderr)
	}
}

func TestExecdStartupFailures(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		args []string
		want int
	}{
		{"missing entrypoint", nil, []string{"--addr", "127.0.0.1:0", "--", "sbx-execd-no-such-binary"}, 127},
		{"bad grace", []string{EnvGraceShutdown + "=soon"}, []string{"--addr", "127.0.0.1:0"}, 2},
		{"bad flag", nil, []string{"--nope"}, 2},
		{"no listener", nil, []string{"--addr", ""}, 2},
		{"vsock port any", nil, []string{"--addr", "127.0.0.1:0", "--vsock-port", "4294967295"}, 2},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, stderr := startExecd(t, c.env, c.args...)
			if code := waitExit(t, cmd, 20*time.Second); code != c.want {
				t.Fatalf("exit %d, want %d; stderr:\n%s", code, c.want, stderr)
			}

			if stderr.Len() == 0 {
				t.Error("failed without saying why")
			}
		})
	}
}

// reaperScenario starts many short-lived children through a reaping table at once and checks
// that every exit code arrives intact - the property a wait4(-1) reaper most easily breaks.
func reaperScenario() int {
	t := newProcs(true)
	defer t.close()

	const n = 40

	var wg sync.WaitGroup

	errs := make(chan string, n)

	for i := range n {
		wg.Add(1)

		go func() {
			defer wg.Done()

			p, err := t.start(exec.Command("sh", "-c", fmt.Sprintf("exit %d", i%50)))
			if err != nil {
				errs <- err.Error()
				return
			}

			select {
			case <-p.done:
			case <-time.After(10 * time.Second):
				errs <- fmt.Sprintf("child %d never reaped", i)
				return
			}

			if p.exitCode() != i%50 {
				errs <- fmt.Sprintf("child %d exited %d", i, p.exitCode())
			}
		}()
	}

	wg.Wait()
	close(errs)

	failed := 0
	for e := range errs {
		fmt.Fprintln(os.Stderr, e)
		failed++
	}

	if failed > 0 {
		return 1
	}

	return 0
}

func TestReaperDeliversEveryExitCode(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "SBX_EXECD_TEST_MAIN=reaper")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
