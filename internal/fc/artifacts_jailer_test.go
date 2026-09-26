package fc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The jailer ships in the same release tarball as firecracker, so it is pinned by the same hash:
// the tarball's. It is extracted beside nothing of the firecracker cache's, so a v0.12 cache
// (firecracker only) is not mistaken for one holding it.
func TestTheJailerIsPinnedByTheReleaseTarball(t *testing.T) {
	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for name, body := range map[string]string{"release-x/firecracker-x": "#!fc", "release-x/jailer-x": "#!jailer"} {
		must(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}))
		_, _ = tw.Write([]byte(body))
	}

	tw.Close()
	gz.Close()

	tgz := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) }))
	t.Cleanup(srv.Close)

	pin := Pin{FirecrackerURL: srv.URL + "/fc.tgz", FirecrackerSHA256: sum(tgz),
		FirecrackerMember: "release-x/firecracker-x", JailerMember: "release-x/jailer-x"}

	j, err := cacheFor(t.TempDir(), pin, nil).ResolveJailer(context.Background(), "arm64")
	if err != nil {
		t.Fatal(err)
	}

	if b, _ := os.ReadFile(j); string(b) != "#!jailer" {
		t.Fatalf("jailer = %q", b)
	}

	if st, _ := os.Stat(j); st.Mode()&0o111 == 0 {
		t.Fatal("the jailer is not executable")
	}

	pin.FirecrackerSHA256 = strings.Repeat("0", 64)
	if _, err := cacheFor(t.TempDir(), pin, nil).ResolveJailer(context.Background(), "arm64"); !errors.Is(err, ErrChecksum) {
		t.Fatalf("a tarball that is not the pinned one: %v", err)
	}

	// SBX_FC_JAILER_BINARY is used as given; an unpinned arch without it is refused by name.
	own := filepath.Join(t.TempDir(), "jailer")
	must(t, os.WriteFile(own, []byte("x"), 0o755))

	c := &ArtifactCache{Dir: t.TempDir(), Pins: map[string]Pin{}, Getenv: func(k string) string {
		return map[string]string{JailerBinaryEnv: own}[k]
	}}

	if got, err := c.ResolveJailer(context.Background(), "riscv64"); err != nil || got != own {
		t.Fatalf("override: %q, %v", got, err)
	}

	c.Getenv = func(string) string { return "" }
	if _, err := c.ResolveJailer(context.Background(), "riscv64"); err == nil || !strings.Contains(err.Error(), JailerBinaryEnv) {
		t.Fatalf("unpinned: %v", err)
	}
}

func TestEveryPinNamesItsJailer(t *testing.T) {
	for arch, p := range Pins {
		dir, fc, _ := strings.Cut(p.FirecrackerMember, "/")
		if p.JailerMember != dir+"/"+strings.Replace(fc, "firecracker-", "jailer-", 1) {
			t.Errorf("%s: jailer member %q is not firecracker's sibling in %s", arch, p.JailerMember, dir)
		}
	}
}
