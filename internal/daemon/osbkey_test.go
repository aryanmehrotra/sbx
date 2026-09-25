package daemon

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/osb"
)

// Loopback is not private: on a VM-backed engine (colima, Docker Desktop) every container
// reaches the host's 127.0.0.1 through host.docker.internal and friends. So `--osb-addr` with no
// key must still require one - generated, stored where a local client finds it - and keyless is
// something the operator types, never a default.
func TestOpenSandboxAPIRequiresAKeyByDefault(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SBX_OSB_KEY", "")

	d := New(&listingProvider{}, time.Minute, time.Second, time.Hour)

	api, ln, err := openAPI(d, "127.0.0.1:0", "")
	if err != nil {
		t.Fatal(err)
	}

	defer ln.Close()
	defer api.Close()

	keyFile := filepath.Join(os.Getenv("HOME"), ".sbx", "osb", "key")

	body, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("no key file was written at %s: %v", keyFile, err)
	}

	key := strings.TrimSpace(string(body))

	get := func(k string) int {
		req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil)
		if k != "" {
			req.Header.Set("OPEN-SANDBOX-API-KEY", k)
		}

		rec := httptest.NewRecorder()
		api.Handler().ServeHTTP(rec, req)

		return rec.Code
	}

	if got := get(""); got != http.StatusUnauthorized {
		t.Fatalf("a keyless request to a loopback API started without --osb-key = %d, want 401", got)
	}

	if got := get(key); got != http.StatusOK {
		t.Fatalf("the generated key was refused: %d", got)
	}

	// Restarting reuses it, so a client configured from the file keeps working.
	d2 := New(&listingProvider{}, time.Minute, time.Second, time.Hour)

	api2, ln2, err := openAPI(d2, "127.0.0.1:0", "")
	if err != nil {
		t.Fatal(err)
	}

	defer ln2.Close()
	defer api2.Close()

	if again, _ := os.ReadFile(keyFile); strings.TrimSpace(string(again)) != key {
		t.Fatal("a restart replaced the stored key")
	}
}

func TestOpenSandboxAPIInsecureNoKeyIsExplicitAndLoopbackOnly(t *testing.T) {
	log.SetOutput(io.Discard)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SBX_OSB_KEY", "")

	d := New(&listingProvider{}, time.Minute, time.Second, time.Hour)
	d.osbNoKey = true

	api, ln, err := openAPI(d, "127.0.0.1:0", "")
	if err != nil {
		t.Fatal(err)
	}

	defer ln.Close()
	defer api.Close()

	rec := httptest.NewRecorder()
	api.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/sandboxes", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("--osb-insecure-no-key still demanded a key: %d", rec.Code)
	}

	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".sbx", "osb", "key")); err == nil {
		t.Fatal("--osb-insecure-no-key wrote a key file nobody will be asked for")
	}

	// Both at once is a contradiction to report, not a precedence to guess.
	if _, _, err := openAPI(d, "127.0.0.1:0", "k"); err == nil ||
		!strings.Contains(err.Error(), "--osb-insecure-no-key") {
		t.Fatalf("--osb-key with --osb-insecure-no-key = %v, want a refusal", err)
	}

	if _, _, err := openAPI(d, "0.0.0.0:0", ""); err == nil {
		t.Fatal("--osb-insecure-no-key was accepted on a non-loopback address")
	}
}

// openAPI calls openSandboxAPI with an address and a key and zero values for everything after
// them. These fixes ship as a patch on v0.9.0, whose openSandboxAPI is (addr, key, scope), as
// well as on main, where it also takes host paths; going through reflect keeps one test file
// that compiles on both.
func openAPI(d *daemon, addr, key string) (*osb.Server, net.Listener, error) {
	f := reflect.ValueOf(d.openSandboxAPI)
	args := []reflect.Value{reflect.ValueOf(addr), reflect.ValueOf(key)}

	for i := 2; i < f.Type().NumIn(); i++ {
		args = append(args, reflect.Zero(f.Type().In(i)))
	}

	out := f.Call(args)

	api, _ := out[0].Interface().(*osb.Server)
	ln, _ := out[1].Interface().(net.Listener)
	err, _ := out[2].Interface().(error)

	return api, ln, err
}
