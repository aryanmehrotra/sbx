package daemon

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/agentbin"
	"github.com/aryanmehrotra/sbx/internal/logs"
	"io"
	"log"
	"testing"
	"time"
)

// Serving the API starts the agent build, so a source build's first create does not pay for a
// ~50s cross-compile inside the client's ready timeout. A refused start builds nothing.
func TestServingTheAPIWarmsTheAgent(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SBX_OSB_KEY", "")

	warmed := make(chan string, 4)

	saved := warmAgent
	warmAgent = func(version string) { warmed <- version }
	t.Cleanup(func() { warmAgent = saved })

	if _, _, err := openAPI(New(&listingProvider{}, time.Minute, time.Second, time.Hour), "0.0.0.0:0", ""); err == nil {
		t.Fatal("a non-loopback --osb-addr was not refused")
	}

	select {
	case v := <-warmed:
		t.Fatalf("a refused start began an agent build (%q)", v)
	case <-time.After(100 * time.Millisecond):
	}

	api, ln, err := openAPI(New(&listingProvider{}, time.Minute, time.Second, time.Hour), "127.0.0.1:0", "")
	if err != nil {
		t.Fatal(err)
	}

	defer ln.Close()
	defer api.Close()

	select {
	case <-warmed:
	case <-time.After(2 * time.Second):
		t.Fatal("serving the API did not start the agent build")
	}
}

// A daemon serving the API where the agent cannot be built used to log nothing at start, so the
// first sign was every API create failing later. The warm-up is where that is first known.
func TestWarmAgentWarnsWhenTheAgentCannotBeFound(t *testing.T) {
	var buf bytes.Buffer

	saved := logs.Default.SetOutput(&buf)
	t.Cleanup(func() { logs.Default.SetOutput(saved) })

	warmAgentWith("dev", func(context.Context, string, string) (agentbin.Source, error) {
		return agentbin.Source{}, errors.New("no linux/arm64 sbx binary to run as the sandbox agent")
	})

	got := buf.String()
	for _, want := range []string{"WARN", "no linux/arm64 sbx binary", "SBX_EXECD_BINARY",
		"SBX_SOURCE_DIR", "release"} {
		if !strings.Contains(got, want) {
			t.Errorf("the start log %q does not mention %s", got, want)
		}
	}
}

// Found, or a release (which uses its published image and never calls Locate): nothing to say.
func TestWarmAgentIsQuietWhenTheAgentIsFound(t *testing.T) {
	var buf bytes.Buffer

	saved := logs.Default.SetOutput(&buf)
	t.Cleanup(func() { logs.Default.SetOutput(saved) })

	warmAgentWith("dev", func(context.Context, string, string) (agentbin.Source, error) {
		return agentbin.Source{File: "/x/sbx"}, nil
	})

	warmAgentWith("v1.2.3", func(context.Context, string, string) (agentbin.Source, error) {
		t.Error("a release build looked for a source build")
		return agentbin.Source{}, errors.New("unreachable")
	})

	if buf.Len() != 0 {
		t.Errorf("logged %q with nothing wrong", buf.String())
	}
}
