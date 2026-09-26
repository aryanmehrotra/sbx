package fc

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// console.log is the guest's serial console, appended to by firecracker for the VM's whole life.
// Unbounded, a guest printing in a loop fills the host's disk. Capped, the newest output is kept
// in whole lines, and a writer holding the file O_APPEND keeps appending after the trim.
func TestCapFileKeepsTheNewestWholeLinesAndTheAppenderStillAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), ConsoleName)

	w, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // as firecracker has it
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	for i := range 200 {
		if _, err := w.WriteString(strings.Repeat("x", 40) + " line " + string(rune('a'+i%26)) + "\n"); err != nil {
			t.Fatal(err)
		}
	}

	if err := capFile(path, 4096, 1024); err != nil {
		t.Fatal(err)
	}

	b, _ := os.ReadFile(path)
	if len(b) > 1024+128 {
		t.Fatalf("capped to %d bytes, want about 1 KiB", len(b))
	}

	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if !strings.Contains(lines[0], "trimmed") {
		t.Fatalf("the trim is not said: %q", lines[0])
	}

	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, strings.Repeat("x", 40)) {
			t.Fatalf("a partial line was kept: %q", l)
		}
	}

	if !strings.HasSuffix(string(b), "line "+string(rune('a'+199%26))+"\n") {
		t.Fatalf("the newest line was not kept: %q", b[len(b)-60:])
	}

	if _, err := w.WriteString("after\n"); err != nil {
		t.Fatal(err)
	}

	b, _ = os.ReadFile(path)
	if !bytes.HasSuffix(b, []byte("line "+string(rune('a'+199%26))+"\nafter\n")) || bytes.Contains(b, []byte{0}) {
		t.Fatalf("the appender's next write did not follow the kept tail: %q", b[max(0, len(b)-80):])
	}

	// Under the cap, and absent: untouched, no error.
	small := filepath.Join(t.TempDir(), "small")
	_ = os.WriteFile(small, []byte("hi\n"), 0o600)

	if err := capFile(small, 4096, 1024); err != nil {
		t.Fatal(err)
	}

	if b, _ := os.ReadFile(small); string(b) != "hi\n" {
		t.Fatalf("a small file changed: %q", b)
	}

	if err := capFile(filepath.Join(t.TempDir(), "none"), 4096, 1024); err != nil {
		t.Fatalf("an absent file: %v", err)
	}
}
