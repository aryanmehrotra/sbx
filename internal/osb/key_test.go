package osb

import (
	"os"
	"path/filepath"
	"testing"
)

// The generated key is a credential for every sandbox on the machine, so it is written 0600 in a
// 0700 directory, reused across restarts (clients configured from the file keep working), and
// never replaced behind the operator's back.
func TestLoadOrCreateKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "osb")

	key, path, created, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !created || len(key) < 32 {
		t.Fatalf("first call: created=%v key len %d, want a new key of at least 32 chars", created, len(key))
	}

	if path != filepath.Join(dir, "key") {
		t.Fatalf("key stored at %s, want %s", path, filepath.Join(dir, "key"))
	}

	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}

		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s is %o, want %o", p, got, want)
		}
	}

	again, _, created, err := LoadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}

	if created || again != key {
		t.Fatalf("second call minted a new key (created=%v): a restart must reuse the stored one", created)
	}

	if got := ReadKey(dir); got != key {
		t.Fatalf("ReadKey = %q, want the stored key", got)
	}

	// A key file somebody else can read is tightened, not trusted as is.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := LoadOrCreateKey(dir); err != nil {
		t.Fatal(err)
	}

	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("a world-readable key file was left %o", st.Mode().Perm())
	}

	// An empty file is not "no key": it is a mistake to report, never a keyless server.
	if err := os.WriteFile(path, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := LoadOrCreateKey(dir); err == nil {
		t.Fatal("an empty key file was accepted")
	}

	if got := ReadKey(filepath.Join(t.TempDir(), "absent")); got != "" {
		t.Fatalf("ReadKey of a missing file = %q, want empty", got)
	}
}
