package agentbin

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/logs"
)

// stubCompile swaps crossCompile for f and forgets every build this process has made.
func stubCompile(t *testing.T, f func(context.Context, string, string, string, string) (string, error)) {
	t.Helper()

	saved := crossCompile
	crossCompile = f
	forgetBuilds()

	t.Cleanup(func() {
		crossCompile = saved
		forgetBuilds()
	})
}

func otherArch() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}

	return "arm64"
}

// A source build's first API sandbox waited for a cross-compile of the agent - about 50s on a
// cold go cache - inside a client's 30s ready timeout, and nothing was logged. The daemon now
// starts that build when it starts serving the API, so it has to be one build: every caller
// that arrives while it runs, or after it finished, gets that binary rather than a second
// compile. And it is said at INFO, both ends, with how long it took.
func TestTheAgentIsCompiledOncePerProcess(t *testing.T) {
	t.Setenv("SBX_EXECD_BINARY", "")

	if _, ok := FindSource(); !ok {
		t.Skip("needs the checkout")
	}

	var n atomic.Int32

	release := make(chan struct{})

	stubCompile(t, func(context.Context, string, string, string, string) (string, error) {
		n.Add(1)
		<-release

		return "/built/sbx", nil
	})

	var buf bytes.Buffer

	prev := logs.Default
	logs.Default = logs.New(&buf)
	t.Cleanup(func() { logs.Default = prev })

	var wg sync.WaitGroup

	for range 5 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if src, err := Locate(context.Background(), otherArch(), "dev"); err != nil || src.File != "/built/sbx" {
				t.Errorf("Locate = %+v, %v", src, err)
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if _, err := Locate(context.Background(), otherArch(), "dev"); err != nil {
		t.Fatal(err)
	}

	if got := n.Load(); got != 1 {
		t.Fatalf("compiled %d times for six callers, want once", got)
	}

	out := buf.String()
	if !strings.Contains(out, "building the sandbox agent") || !strings.Contains(out, "built the sandbox agent") {
		t.Errorf("the one-time build was not logged at both ends:\n%s", out)
	}
}

// A failed build is not remembered: the next caller - after the user fixed their checkout, say -
// tries again rather than inheriting the failure for the life of the daemon.
func TestAFailedAgentBuildIsRetried(t *testing.T) {
	t.Setenv("SBX_EXECD_BINARY", "")

	if _, ok := FindSource(); !ok {
		t.Skip("needs the checkout")
	}

	var n atomic.Int32

	stubCompile(t, func(context.Context, string, string, string, string) (string, error) {
		if n.Add(1) == 1 {
			return "", errors.New("syntax error")
		}

		return "/built/sbx", nil
	})

	if _, err := Locate(context.Background(), otherArch(), "dev"); err == nil {
		t.Fatal("a failed build was reported as a binary")
	}

	if src, err := Locate(context.Background(), otherArch(), "dev"); err != nil || src.File != "/built/sbx" {
		t.Fatalf("the retry after a failed build = %+v, %v", src, err)
	}
}
