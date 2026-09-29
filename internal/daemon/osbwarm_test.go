package daemon

import (
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
