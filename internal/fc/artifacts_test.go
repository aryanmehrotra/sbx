package fc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func tgzWith(t *testing.T, name string, content []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, f := range []struct {
		name string
		body []byte
	}{{"release-x/NOTICE", []byte("n")}, {name, content}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body))}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}

	tw.Close()
	gz.Close()

	return buf.Bytes()
}

type artifactServer struct {
	*httptest.Server
	hits atomic.Int32
	pin  Pin
}

func newArtifactServer(t *testing.T) *artifactServer {
	t.Helper()

	kernel := []byte("MZ-a-kernel")
	tgz := tgzWith(t, "release-x/firecracker-x", []byte("#!fc"))

	as := &artifactServer{}
	as.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.hits.Add(1)

		switch r.URL.Path {
		case "/fc.tgz":
			_, _ = w.Write(tgz)
		case "/vmlinux":
			_, _ = w.Write(kernel)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(as.Close)

	as.pin = Pin{
		FirecrackerURL: as.URL + "/fc.tgz", FirecrackerSHA256: sum(tgz), FirecrackerMember: "release-x/firecracker-x",
		KernelURL: as.URL + "/vmlinux", KernelSHA256: sum(kernel),
	}

	return as
}

func cacheFor(dir string, pin Pin, env map[string]string) *ArtifactCache {
	return &ArtifactCache{
		Dir: dir, HTTP: http.DefaultClient,
		Getenv: func(k string) string { return env[k] },
		Pins:   map[string]Pin{"arm64": pin},
	}
}

func TestArtifactsFetchVerifyAndCache(t *testing.T) {
	as := newArtifactServer(t)
	dir := t.TempDir()
	c := cacheFor(dir, as.pin, nil)

	a, err := c.Resolve(context.Background(), "arm64")
	if err != nil {
		t.Fatal(err)
	}

	if b, _ := os.ReadFile(a.Firecracker); string(b) != "#!fc" {
		t.Fatalf("binary = %q", b)
	}

	if st, _ := os.Stat(a.Firecracker); st.Mode()&0o111 == 0 {
		t.Fatalf("binary not executable: %v", st.Mode())
	}

	if b, _ := os.ReadFile(a.Kernel); string(b) != "MZ-a-kernel" {
		t.Fatalf("kernel = %q", b)
	}

	for _, p := range []string{a.Firecracker, a.Kernel} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(p), builtSentinel)); err != nil {
			t.Fatalf("no sentinel beside %s", p)
		}
	}

	before := as.hits.Load()

	if _, err := c.Resolve(context.Background(), "arm64"); err != nil {
		t.Fatal(err)
	}

	if as.hits.Load() != before {
		t.Fatal("a cached artifact was downloaded again")
	}

	// No temp directories left behind.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("stray %s", e.Name())
		}
	}
}

func TestArtifactChecksumMismatchIsRefusedAndNotCached(t *testing.T) {
	as := newArtifactServer(t)
	pin := as.pin
	pin.KernelSHA256 = strings.Repeat("0", 64)

	dir := t.TempDir()

	_, err := cacheFor(dir, pin, nil).Resolve(context.Background(), "arm64")
	if !errors.Is(err, ErrChecksum) || !strings.Contains(err.Error(), "will not run it") {
		t.Fatalf("err = %v", err)
	}

	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "vmlinux") || strings.HasPrefix(e.Name(), ".tmp-vmlinux") {
			t.Fatalf("a refused kernel left %s", e.Name())
		}
	}
}

func TestArtifactHalfBuiltFinalIsRebuilt(t *testing.T) {
	as := newArtifactServer(t)
	dir := t.TempDir()

	// A final directory with no sentinel: what a crash would leave if the order were wrong.
	stale := filepath.Join(dir, "vmlinux-"+KernelVersion+"-arm64-"+shortSHA(as.pin.KernelSHA256))
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(stale, "vmlinux"), []byte("half"), 0o644); err != nil {
		t.Fatal(err)
	}

	a, err := cacheFor(dir, as.pin, nil).Resolve(context.Background(), "arm64")
	if err != nil {
		t.Fatal(err)
	}

	if b, _ := os.ReadFile(a.Kernel); string(b) != "MZ-a-kernel" {
		t.Fatalf("trusted a half-built kernel: %q", b)
	}
}

func TestArtifactOverridesNeedNoNetwork(t *testing.T) {
	dir := t.TempDir()
	bin, kern := filepath.Join(dir, "fc"), filepath.Join(dir, "k")

	for _, p := range []string{bin, kern} {
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// An unpinned architecture with both overrides works, and nothing is fetched.
	c := &ArtifactCache{Dir: dir, Getenv: func(k string) string {
		return map[string]string{"SBX_FC_BINARY": bin, "SBX_FC_KERNEL": kern}[k]
	}, Pins: map[string]Pin{}}

	a, err := c.Resolve(context.Background(), "riscv64")
	if err != nil {
		t.Fatal(err)
	}

	if a.Firecracker != bin || a.Kernel != kern {
		t.Fatalf("artifacts = %+v", a)
	}

	// Without them it is refused, naming the variables.
	c.Getenv = func(string) string { return "" }

	if _, err := c.Resolve(context.Background(), "riscv64"); err == nil || !strings.Contains(err.Error(), "SBX_FC_KERNEL") {
		t.Fatalf("unpinned arch = %v", err)
	}

	// An override that is not a file is refused rather than handed to the VMM.
	c.Getenv = func(k string) string {
		if k == "SBX_FC_BINARY" {
			return dir
		}

		return ""
	}

	if _, err := c.Resolve(context.Background(), "riscv64"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory override = %v", err)
	}
}

func TestArtifactMissingMemberSaysSo(t *testing.T) {
	as := newArtifactServer(t)
	pin := as.pin
	pin.FirecrackerMember = "release-x/renamed"

	_, err := cacheFor(t.TempDir(), pin, nil).Resolve(context.Background(), "arm64")
	if err == nil || !strings.Contains(err.Error(), "release layout changed") {
		t.Fatalf("err = %v", err)
	}
}

func TestPinsAreComplete(t *testing.T) {
	for arch, p := range Pins {
		for name, v := range map[string]string{"fc sha": p.FirecrackerSHA256, "kernel sha": p.KernelSHA256} {
			if len(v) != 64 || strings.Trim(v, "0123456789abcdef") != "" {
				t.Errorf("%s %s = %q is not a sha256", arch, name, v)
			}
		}

		if !strings.Contains(p.FirecrackerURL, FirecrackerVersion) || !strings.Contains(p.KernelURL, KernelVersion) {
			t.Errorf("%s urls do not carry the pinned versions: %+v", arch, p)
		}
	}
}
