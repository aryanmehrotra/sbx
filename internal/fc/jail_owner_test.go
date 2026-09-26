package fc

import (
	"os"
	"path/filepath"
	"testing"
)

// asRoot runs PrepareJail's ownership as root would, recording every chown instead of making it.
func asRoot(t *testing.T) map[string][2]int {
	t.Helper()

	got := map[string][2]int{}
	e, c := geteuid, chown
	geteuid = func() int { return 0 }
	chown = func(p string, uid, gid int) error {
		got[filepath.Base(p)] = [2]int{uid, gid}
		return nil
	}

	t.Cleanup(func() { geteuid, chown = e, c })

	return got
}

// A read-only volume is never given to the jail's uid: a VMM owning the file could rewrite the
// "read-only" volume for whichever sandbox attaches it next. It is left root's - taken back from
// a uid a previous read-write attach gave it to - and readable, not writable, by anyone else, so
// the jailed VMM can still open it read-only. A read-write drive is the VM's, as before.
func TestAReadOnlyDriveIsNeverGivenToTheJailUID(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "vm")
	must(t, os.MkdirAll(dir, 0o700))

	ro, rw := filepath.Join(base, "ro.ext4"), filepath.Join(base, "rw.ext4")
	must(t, os.WriteFile(ro, []byte("ro"), 0o660))
	must(t, os.Chmod(ro, 0o660))
	must(t, os.WriteFile(rw, []byte("rw"), 0o600))

	const uid = DefaultJailUIDBase + 7

	chowns := asRoot(t)

	s := LaunchSpec{Binary: filepath.Join(base, "firecracker"), Dir: dir, Jail: &JailSpec{
		Jailer: "/nonexistent/jailer", UID: uid, GID: uid,
		Files: []Stage{
			{Name: "vol0.ext4", Host: ro, ReadOnly: true},
			{Name: "vol1.ext4", Host: rw},
		},
	}}

	if _, err := PrepareJail(s); err != nil {
		t.Fatal(err)
	}

	owner := func(name string) ([2]int, bool) {
		o, ok := chowns[name]
		return o, ok
	}

	if o, ok := owner("vol1.ext4"); !ok || o != [2]int{uid, uid} {
		t.Fatalf("the read-write volume went to %v (chowned %v), want the VM's uid %d", o, ok, uid)
	}

	if o, ok := owner("vol0.ext4"); ok && (o[0] != 0 || o[1] != 0) {
		t.Fatalf("the read-only volume was given to %v; want it root's", o)
	} else if !ok {
		t.Fatal("the read-only volume was not taken back to root: a previous read-write attach's uid keeps it")
	}

	st, err := os.Stat(ro)
	must(t, err)

	if m := st.Mode().Perm(); m&0o004 == 0 || m&0o022 != 0 {
		t.Fatalf("the read-only volume is %v: the jail uid must be able to read it and nobody but root write it", m)
	}
}
