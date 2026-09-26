package fc

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The jailer is on unless the operator says otherwise, and a value it does not understand is
// refused rather than read as either answer: a typo in an escape hatch must not open it.
func TestJailFromEnvDefaultsOnAndRefusesWhatItCannotRead(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}

		return func(k string) string { return m[k] }
	}

	c, err := JailFromEnv(env())
	if err != nil || c == nil || c.UIDBase != DefaultJailUIDBase {
		t.Fatalf("default = %+v, %v; want on with base %d", c, err, DefaultJailUIDBase)
	}

	for _, on := range []string{"on", "1", "true", "ON"} {
		if c, err := JailFromEnv(env(JailerEnv, on)); err != nil || c == nil {
			t.Fatalf("%s=%s: %+v, %v", JailerEnv, on, c, err)
		}
	}

	for _, off := range []string{"off", "0", "false", "OFF"} {
		if c, err := JailFromEnv(env(JailerEnv, off)); err != nil || c != nil {
			t.Fatalf("%s=%s: %+v, %v; want off", JailerEnv, off, c, err)
		}
	}

	if _, err := JailFromEnv(env(JailerEnv, "of")); err == nil || !strings.Contains(err.Error(), JailerEnv) {
		t.Fatalf("a typo was accepted: %v", err)
	}

	if c, err := JailFromEnv(env(JailUIDBaseEnv, "2000000")); err != nil || c.UIDBase != 2000000 {
		t.Fatalf("base override: %+v, %v", c, err)
	}

	// 0 would make slot 0's first VM root; a negative or a range past uid_t is not a uid at all.
	for _, bad := range []string{"0", "-5", "abc", "4294967000"} {
		if _, err := JailFromEnv(env(JailUIDBaseEnv, bad)); err == nil {
			t.Fatalf("%s=%s accepted", JailUIDBaseEnv, bad)
		}
	}
}

// A VM's uid is arithmetic on its address, like its IP: every sbx process agrees on it without
// a table, a re-created VM gets the same one back, and no two VMs on the host share one.
func TestJailUIDIsPerVMArithmeticAndNeverRoot(t *testing.T) {
	c := JailConfig{UIDBase: DefaultJailUIDBase}
	seen := map[int]Addr{}

	for slot := 0; slot <= 253; slot += 23 {
		for index := 0; index <= 250; index += 17 {
			a := Addr{Slot: slot, Index: index}
			u := c.UID(a)

			if u <= 0 || u < c.UIDBase || u >= c.UIDBase+JailUIDSpan {
				t.Fatalf("%+v -> uid %d, outside [%d, %d)", a, u, c.UIDBase, c.UIDBase+JailUIDSpan)
			}

			if prev, dup := seen[u]; dup {
				t.Fatalf("%+v and %+v share uid %d", a, prev, u)
			}

			seen[u] = a
		}
	}

	// The scheme as documented (SECURITY.md), in literal numbers: arithmetic the test does itself
	// would move with the code.
	for a, want := range map[Addr]int{{Slot: 0, Index: 0}: 900000, {Slot: 1, Index: 2}: 900258, {Slot: 253, Index: 250}: 965018} {
		if got := c.UID(a); got != want {
			t.Fatalf("UID(%+v) = %d, want %d (900000 + slot*256 + index)", a, got, want)
		}
	}
}

func TestJailerArgvIsTheV117Interface(t *testing.T) {
	dir := "/state/vms/0123456789abcdef"
	s := LaunchSpec{Binary: "/cache/firecracker", Dir: dir, ID: "ignored-when-jailed", Jail: &JailSpec{
		Jailer: "/cache/jailer", UID: 900517, GID: 900517, CPUs: 2, MemMiB: 512,
	}}

	got := JailerArgs(s)
	id := JailID(dir)

	want := []string{
		"--id", id,
		"--exec-file", "/cache/firecracker",
		"--uid", "900517", "--gid", "900517",
		"--chroot-base-dir", dir + "/jail",
		"--cgroup-version", "2",
		"--parent-cgroup", JailCgroupParent,
		"--cgroup", "cpu.max=200000 100000",
		"--cgroup", "memory.max=" + itoa((512+JailMemOverheadMiB)<<20),
		"--",
		"--api-sock", "/" + APISockName,
	}

	if !slices.Equal(got, want) {
		t.Fatalf("argv\n got %q\nwant %q", got, want)
	}

	// No limits: no cgroup at all. With --cgroup-version 2, no --cgroup and an existing
	// --parent-cgroup, v1.17's jailer MOVES the process into that parent, which fails once a
	// sibling VM has enabled memory in its subtree_control ("no internal processes").
	s.Jail.CPUs, s.Jail.MemMiB = 0, 0
	for _, a := range JailerArgs(s) {
		if strings.HasPrefix(a, "--cgroup") || a == "--parent-cgroup" {
			t.Fatalf("no limits still passed %q: %q", a, JailerArgs(s))
		}
	}
}

func TestJailIDIsValidForTheJailerAndUniquePerDirectory(t *testing.T) {
	a, b := JailID("/root/.sbx/fc/vms/0123456789abcdef"), JailID("/other/.sbx/fc/vms/0123456789abcdef")

	if a == b {
		t.Fatal("two state roots with the same ref share a jail id, and so a cgroup")
	}

	for _, id := range []string{a, b} {
		if len(id) > 64 || strings.Trim(id, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			t.Fatalf("%q: the jailer takes alphanumerics and hyphens, at most 64", id)
		}
	}
}

// What the VMM is given for each file, and where what it writes lands.
func TestTheViewMapsHostPathsIntoTheChroot(t *testing.T) {
	var off View

	if got := off.Path("/state/vms/x/rootfs.ext4"); got != "/state/vms/x/rootfs.ext4" {
		t.Fatalf("unjailed Path = %q; an unjailed VMM sees the host's own paths", got)
	}

	on := View{Root: "/state/vms/x/jail/firecracker/sbx-1/root"}

	for host, want := range map[string]string{
		"/state/vms/x/rootfs.ext4":        "/rootfs.ext4",
		"/state/vms/x/vm.state.new":       "/vm.state.new",
		"/state/snapshots/img-1/vm.state": "/vm.state",
	} {
		if got := on.Path(host); got != want {
			t.Fatalf("Path(%s) = %q, want %q", host, got, want)
		}
	}

	if got := on.At("/state/volumes/data.ext4", "vol0.ext4"); got != "/vol0.ext4" {
		t.Fatalf("At = %q", got)
	}
}

// PrepareJail makes the root the jailer chroots into: the VM's own files hard-linked (the same
// inode, so what the guest writes to its disk is on the VM's disk) and given to the VM's uid; a
// shared file (the kernel) linked only when anyone may read it, and never re-owned; and the two
// sockets reachable from the VM directory at the names every host-side caller already uses.
func TestPrepareJailStagesByLinkAndPointsTheSocketsIn(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "vm")
	must(t, os.MkdirAll(dir, 0o700))

	write := func(p string, mode os.FileMode) string {
		must(t, os.WriteFile(p, []byte(filepath.Base(p)), mode))
		must(t, os.Chmod(p, mode))

		return p
	}

	rootfs := write(filepath.Join(dir, RootfsName), 0o600)
	kernel := write(filepath.Join(base, "vmlinux"), 0o644)
	private := write(filepath.Join(base, "private-kernel"), 0o600)
	bin := write(filepath.Join(base, "firecracker"), 0o755)

	s := LaunchSpec{Binary: bin, Dir: dir, Jail: &JailSpec{
		Jailer: "/nonexistent/jailer", UID: os.Getuid(), GID: os.Getgid(),
		Files: []Stage{
			{Name: RootfsName, Host: rootfs},
			{Name: "vmlinux", Host: kernel, Shared: true},
			{Name: "other-kernel", Host: private, Shared: true},
		},
	}}

	root, err := PrepareJail(s)
	if err != nil {
		t.Fatal(err)
	}

	if want := filepath.Join(dir, JailDirName, "firecracker", JailID(dir), "root"); root != want {
		t.Fatalf("root = %s, want %s (the jailer's <base>/<exec name>/<id>/root)", root, want)
	}

	same := func(a, b string) bool {
		x, err1 := os.Stat(a)
		y, err2 := os.Stat(b)

		return err1 == nil && err2 == nil && os.SameFile(x, y)
	}

	if !same(rootfs, filepath.Join(root, RootfsName)) {
		t.Fatal("the rootfs was not hard-linked: the guest would write to a copy the host never sees")
	}

	if !same(kernel, filepath.Join(root, "vmlinux")) {
		t.Fatal("a world-readable shared kernel was copied instead of linked")
	}

	if same(private, filepath.Join(root, "other-kernel")) {
		t.Fatal("a shared file only root may read was linked: re-owning it would hand every VM's uid the original")
	}

	if b, err := os.ReadFile(filepath.Join(root, "other-kernel")); err != nil || string(b) != "private-kernel" {
		t.Fatalf("the copy: %q, %v", b, err)
	}

	for _, name := range []string{APISockName, VsockName} {
		target, err := os.Readlink(filepath.Join(dir, name))
		if err != nil || target != filepath.Join(root, name) {
			t.Fatalf("%s -> %q, %v; want the chroot's %s", name, target, err, name)
		}
	}

	// A second launch in the same directory starts from an empty root: v1.17's jailer mknods
	// /dev/kvm and /dev/net/tun and fails EEXIST on the ones a previous run left.
	stale := filepath.Join(root, "dev", "kvm")
	must(t, os.MkdirAll(filepath.Dir(stale), 0o700))
	must(t, os.WriteFile(stale, nil, 0o600))

	if _, err := PrepareJail(s); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("a previous run's /dev/kvm survived into the next jail: %v", err)
	}

	if !same(rootfs, filepath.Join(root, RootfsName)) {
		t.Fatal("the second launch lost the rootfs link")
	}

	// A staged file that does not exist is the launch's failure, named.
	s.Jail.Files = append(s.Jail.Files, Stage{Name: "vol0.ext4", Host: filepath.Join(base, "gone.ext4")})
	if _, err := PrepareJail(s); err == nil || !strings.Contains(err.Error(), "gone.ext4") {
		t.Fatalf("a missing drive: %v", err)
	}
}

// What the VMM writes in its root is moved into the host's directory, and only if it is a plain
// file of its own: the VMM owns its root, so a compromised one could leave a symlink to a host
// file there, which a host that followed it would then merge, clone or overwrite as root.
func TestAdoptTakesOnlyAPlainFileTheVMMWrote(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	host := filepath.Join(base, "vm")

	must(t, os.MkdirAll(root, 0o700))
	must(t, os.MkdirAll(host, 0o700))

	v := View{Root: root}

	must(t, os.WriteFile(filepath.Join(root, "vm.state.new"), []byte("state"), 0o600))

	if err := v.Adopt(filepath.Join(host, "vm.state.new"), "vm.state.new"); err != nil {
		t.Fatal(err)
	}

	if b, err := os.ReadFile(filepath.Join(host, "vm.state.new")); err != nil || string(b) != "state" {
		t.Fatalf("adopted: %q, %v", b, err)
	}

	if _, err := os.Lstat(filepath.Join(root, "vm.state.new")); !os.IsNotExist(err) {
		t.Fatal("adopting left the VMM's copy behind")
	}

	secret := filepath.Join(base, "shadow")
	must(t, os.WriteFile(secret, []byte("root's"), 0o600))

	for name, plant := range map[string]func(p string) error{
		"symlink":  func(p string) error { return os.Symlink(secret, p) },
		"hardlink": func(p string) error { return os.Link(secret, p) },
		"dir":      func(p string) error { return os.Mkdir(p, 0o700) },
	} {
		must(t, plant(filepath.Join(root, "vm.mem.new")))

		dst := filepath.Join(host, "vm.mem.new")

		err := v.Adopt(dst, "vm.mem.new")
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("%s: Adopt = %v, want refused", name, err)
		}

		if _, err := os.Lstat(dst); !os.IsNotExist(err) {
			t.Fatalf("%s: a refused file was left where the host would use it", name)
		}

		_ = os.RemoveAll(filepath.Join(root, "vm.mem.new"))
	}

	if b, _ := os.ReadFile(secret); string(b) != "root's" {
		t.Fatal("the host file behind the plant was changed")
	}

	// Unjailed, the VMM wrote the host path itself: nothing to move.
	if err := (View{}).Adopt(filepath.Join(host, "nothing"), "nothing"); err != nil {
		t.Fatalf("unjailed Adopt: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
