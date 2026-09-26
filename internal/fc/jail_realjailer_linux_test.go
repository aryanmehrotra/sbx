package fc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestTheRealJailer runs sbx's ExecLauncher against Firecracker v1.17.0's REAL, pinned jailer,
// with this test binary exec'd in firecracker's place (jailedFirecracker) - so it needs root and
// cgroup v2, but no /dev/kvm. It proves what the fakes cannot: the jailer accepts sbx's argv,
// chroots where JailRoot says, execs in place as the VM's uid with no capabilities, applies the
// cgroup limits, and Kill releases the jail and the cgroup; a second launch in the same
// directory works (the jailer's mknod fails on a stale /dev/kvm).
//
// Opt-in: SBX_FC_JAILER_E2E=1, as root, from a compiled test binary (CI's microvm job).
func TestTheRealJailer(t *testing.T) {
	if os.Getenv("SBX_FC_JAILER_E2E") != "1" {
		t.Skip("SBX_FC_JAILER_E2E=1, as root, to run the real jailer")
	}

	if os.Geteuid() != 0 {
		t.Fatal("the jailer needs root")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	state := t.TempDir()
	_ = os.Chmod(state, 0o755)

	jailer, err := NewArtifactCache(filepath.Join(state, "artifacts")).ResolveJailer(ctx, runtime.GOARCH)
	must(t, err)

	self, err := os.Executable()
	must(t, err)

	bin := filepath.Join(state, "firecracker")
	b, err := os.ReadFile(self)
	must(t, err)
	must(t, os.WriteFile(bin, b, 0o755))

	dir := filepath.Join(state, "vms", "0123456789abcdef")
	must(t, os.MkdirAll(dir, 0o700))
	must(t, os.WriteFile(filepath.Join(dir, RootfsName), []byte("disk"), 0o600))

	uid := JailConfig{UIDBase: DefaultJailUIDBase}.UID(Addr{Slot: 2, Index: 3})
	s := LaunchSpec{Binary: bin, Dir: dir, ID: "x", Jail: &JailSpec{
		Jailer: jailer, UID: uid, GID: uid, CPUs: 1, MemMiB: 256,
		Files: []Stage{{Name: RootfsName, Host: filepath.Join(dir, RootfsName)}},
	}}

	for round := 1; round <= 2; round++ {
		start := time.Now()

		pid, err := ExecLauncher{}.Launch(ctx, s)
		if err != nil {
			log, _ := os.ReadFile(filepath.Join(dir, VMMLogName))
			t.Fatalf("round %d: %v\n%s", round, err, log)
		}

		t.Logf("round %d: jailed launch answered in %s", round, time.Since(start))

		status, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
		want := strconv.Itoa(uid)

		for _, l := range strings.Split(string(status), "\n") {
			f := strings.Fields(l)
			switch {
			case len(f) == 5 && (f[0] == "Uid:" || f[0] == "Gid:"):
				for _, id := range f[1:] {
					if id != want {
						t.Fatalf("%s the VMM is not uid %s throughout", l, want)
					}
				}
			case len(f) == 2 && f[0] == "CapEff:" && f[1] != "0000000000000000":
				t.Fatalf("the VMM kept capabilities: %s", l)
			}
		}

		root := "/proc/" + strconv.Itoa(pid) + "/root/"
		if _, err := os.Stat(root + "etc/passwd"); err == nil {
			t.Fatal("the host's /etc is visible to the VMM: not chrooted")
		}

		a, _ := os.Stat(filepath.Join(dir, RootfsName))
		j, _ := os.Stat(root + RootfsName)
		if a == nil || j == nil || !os.SameFile(a, j) {
			t.Fatal("the VMM's /rootfs.ext4 is not the VM's disk")
		}

		cg, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
		rel := strings.TrimPrefix(strings.TrimSpace(string(cg)), "0::")
		if rel != "/"+JailCgroupParent+"/"+JailID(dir) {
			t.Fatalf("cgroup %q, want /%s/%s", rel, JailCgroupParent, JailID(dir))
		}

		mnt := cgroup2Mount()
		if v, _ := os.ReadFile(mnt + rel + "/memory.max"); strings.TrimSpace(string(v)) != strconv.Itoa((256+JailMemOverheadMiB)<<20) {
			t.Fatalf("memory.max = %q", v)
		}

		if v, _ := os.ReadFile(mnt + rel + "/cpu.max"); strings.TrimSpace(string(v)) != "100000 100000" {
			t.Fatalf("cpu.max = %q", v)
		}

		if !(ExecLauncher{}).Alive(dir) {
			t.Fatal("a jailed VMM is not recognised as the directory's")
		}

		must(t, ExecLauncher{}.Kill(ctx, dir))

		if _, err := os.Stat(filepath.Join(dir, JailDirName)); !os.IsNotExist(err) {
			t.Fatal("the jail outlived its VMM")
		}

		if _, err := os.Stat(mnt + rel); !os.IsNotExist(err) {
			t.Fatalf("the cgroup outlived its VMM: %v", err)
		}
	}

	console, _ := os.ReadFile(filepath.Join(dir, ConsoleName))
	if !strings.Contains(string(console), "jailed firecracker uid="+strconv.Itoa(uid)) ||
		strings.Contains(string(console), "host-etc=true") {
		t.Fatalf("console: %q", console)
	}
}
