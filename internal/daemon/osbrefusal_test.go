package daemon

import (
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// taken returns an address somebody is already listening on, for the life of the test.
func taken(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	return ln.Addr().String()
}

// A refused start must leave nothing behind. `sbx serve --osb-addr 0.0.0.0:8080` is refused -
// the API is loopback only - yet it had already minted and written ~/.sbx/osb/key: a credential
// on disk for an API that never ran, which `sbx mcp` would then find and trust. Every refusal
// that does not depend on the key now comes before it.
func TestARefusedOpenSandboxAPIWritesNoKey(t *testing.T) {
	log.SetOutput(io.Discard)

	for _, c := range []struct {
		name string
		addr string
		d    *daemon
	}{
		{"not loopback", "0.0.0.0:0", New(&listingProvider{}, time.Minute, time.Second, time.Hour)},
		{"not host:port", "8080", New(&listingProvider{}, time.Minute, time.Second, time.Hour)},
		{"no runtime", "127.0.0.1:0", New(nil, time.Minute, time.Second, time.Hour)},
		{"port taken", taken(t), New(&listingProvider{}, time.Minute, time.Second, time.Hour)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SBX_OSB_KEY", "")

			if _, _, err := openAPI(c.d, c.addr, ""); err == nil {
				t.Fatalf("--osb-addr %s was not refused", c.addr)
			}

			dir := filepath.Join(os.Getenv("HOME"), ".sbx", "osb")
			if _, err := os.Stat(filepath.Join(dir, "key")); err == nil {
				t.Fatalf("a refused --osb-addr %s still wrote a key in %s", c.addr, dir)
			}
		})
	}
}
