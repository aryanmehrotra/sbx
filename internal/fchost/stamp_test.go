package fchost

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"
)

// After an upgrade the helper VM is still running the old sbx. A command that finds it running
// used to skip Ensure; it now re-Ensures when the VM was last brought up by another build, and
// costs one listing when it was this one.
func TestARunningVMFromAnotherBuildIsBroughtUpToDate(t *testing.T) {
	bin := t.TempDir() + "/sbx"
	if err := os.WriteFile(bin, []byte("new linux sbx"), 0o755); err != nil {
		t.Fatal(err)
	}

	opt := EnsureOptions{Version: "v0.11.0", Binary: func(context.Context) (string, error) { return bin, nil }}

	r := (&fakeRunner{}).on("limactl list", runningLima, nil)
	m := limaManager(t, r)

	if err := m.EnsureCurrent(context.Background(), opt); err != nil {
		t.Fatal(err)
	}

	if all := strings.Join(r.lines(), "\n"); strings.Contains(all, "ssh") {
		t.Fatalf("a VM this build already ensured was touched:\n%s", all)
	}

	// The upgrade.
	m.hostStamp = func(string) string { return "the-next-build" }
	r.calls = nil

	if err := m.EnsureCurrent(context.Background(), opt); err != nil {
		t.Fatal(err)
	}

	if all := strings.Join(r.lines(), "\n"); !strings.Contains(all, "cat > /usr/local/bin/sbx.new") {
		t.Fatalf("a VM running another build's sbx was left as it was:\n%s", all)
	}

	if b, _ := os.ReadFile(m.stampPath()); strings.TrimSpace(string(b)) != "the-next-build" {
		t.Fatalf("stamp after the upgrade = %q", b)
	}
}

// The far side refuses a request from a build that speaks another protocol, by name.
func TestCallRefusesAnotherProtocol(t *testing.T) {
	var out bytes.Buffer

	if err := Call(context.Background(), nil, []string{"list", base64.RawURLEncoding.EncodeToString([]byte(`{"sandbox":"x"}`))},
		nil, &out, nil); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out.String(), "protocol 0") || !strings.Contains(out.String(), "sbx fc vm start") {
		t.Fatalf("answer = %s", out.String())
	}
}
