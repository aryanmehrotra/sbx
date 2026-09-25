package guestinit

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestExecdArgs(t *testing.T) {
	got := execdArgs("/opt/sbx/sbx", "44772", []string{"redis-server", "--save", ""})
	want := []string{"/opt/sbx/sbx", "execd", "--vsock-port", "44772", "--", "redis-server", "--save", ""}

	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}

	// No workload: execd serves alone, and there is no dangling "--".
	if got := execdArgs("/a", "1", nil); slices.Contains(got, "--") {
		t.Fatalf("got %q", got)
	}
}

func TestEnvironDefaults(t *testing.T) {
	got := environ([]string{"PATH=/custom", "A=1"})
	if !slices.Contains(got, "PATH=/custom") || !slices.Contains(got, "HOME=/root") || len(got) != 3 {
		t.Fatalf("got %q", got)
	}

	if got := environ(nil); len(got) != 2 {
		t.Fatalf("empty env got %q", got)
	}
}

func TestMainRefusesOutsideAVM(t *testing.T) {
	// Not PID 1 on linux, not linux elsewhere: either way a refusal, never a mount attempt.
	if Main(nil) == 0 {
		t.Fatal("fc-init ran outside a VM")
	}
}

// docker bind-mounts /etc/hostname, so `docker export` gives an EMPTY file; a VM must fill it,
// or `cat /etc/hostname` in the workload reads nothing (found on a real boot).
func TestWriteHostname(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hostname")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	writeHostname(p, "nginx")

	if b, _ := os.ReadFile(p); string(b) != "nginx\n" {
		t.Fatalf("/etc/hostname = %q", b)
	}

	writeHostname(p, "")

	if b, _ := os.ReadFile(p); string(b) != "nginx\n" {
		t.Fatalf("an empty name rewrote it: %q", b)
	}
}
