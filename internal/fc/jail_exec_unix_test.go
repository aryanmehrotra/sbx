//go:build unix

package fc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecLauncherStartsTheVMMThroughTheJailer(t *testing.T) {
	dir, err := os.MkdirTemp("", "fcj") // short: the API socket's path is capped at 104 bytes on macOS
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })

	argv := filepath.Join(dir, "argv")
	t.Setenv(fakeJailerEnv, argv)

	bin := filepath.Join(dir, "firecracker")
	must(t, os.WriteFile(bin, []byte("fc"), 0o755))

	rootfs := filepath.Join(dir, RootfsName)
	must(t, os.WriteFile(rootfs, []byte("disk"), 0o600))

	s := LaunchSpec{Binary: bin, Dir: dir, ID: "instance", Jail: &JailSpec{
		Jailer: os.Args[0], UID: os.Getuid(), GID: os.Getgid(), CPUs: 1, MemMiB: 128,
		Files: []Stage{{Name: RootfsName, Host: rootfs}},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pid, err := ExecLauncher{}.Launch(ctx, s)
	if err != nil {
		b, _ := os.ReadFile(filepath.Join(dir, VMMLogName))
		t.Fatalf("Launch: %v\n%s", err, b)
	}

	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	b, err := os.ReadFile(argv)
	must(t, err)

	if got, want := strings.Split(string(b), "\x00"), JailerArgs(s); !slices.Equal(got, want) {
		t.Fatalf("the jailer ran with\n %q\nwant\n %q", got, want)
	}

	// The client reached the VMM through <dir>/api.sock: Launch returned only once it answered.
	if _, err := NewClient(filepath.Join(dir, APISockName)).Describe(ctx); err != nil {
		t.Fatalf("the API through the VM directory's socket: %v", err)
	}

	pf, _ := os.ReadFile(filepath.Join(dir, PIDName))
	if !strings.HasPrefix(string(pf), strconv.Itoa(pid)+" ") {
		t.Fatalf("pid file %q, want pid %d: the jailer execs the VMM in place, so they are one", pf, pid)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if c, _ := os.ReadFile(filepath.Join(dir, ConsoleName)); strings.Contains(string(c), "jailed console") {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the jailed VMM's stdout did not reach console.log")
		}

		time.Sleep(10 * time.Millisecond)
	}

	if runtime.GOOS != "linux" {
		return // ownership is read from /proc
	}

	if !(ExecLauncher{}).Alive(dir) {
		t.Fatal("a jailed VMM is not recognised as this directory's: its cmdline names no host socket, only --id")
	}

	must(t, ExecLauncher{}.Kill(ctx, dir))

	if (ExecLauncher{}).Alive(dir) {
		t.Fatal("still alive after Kill")
	}

	if _, err := os.Lstat(filepath.Join(dir, JailDirName)); !os.IsNotExist(err) {
		t.Fatalf("the jail outlived its VMM (its hard links hold a replaced snapshot's blocks): %v", err)
	}

	if st, err := os.Stat(rootfs); err != nil || st.Size() != 4 {
		t.Fatalf("releasing the jail took the VM's own disk: %v", err)
	}
}

// The shared kernel is linked into a root the VM's uid owns, so whatever the jailer does to that
// root after sbx filled it, the kernel must come out of the launch still root's and writable by
// nobody else - it is one inode for every VM on the host.
func TestASharedFileIsNotWritableByTheJailAfterTheLaunch(t *testing.T) {
	dir, err := os.MkdirTemp("", "fcs")
	must(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })

	t.Setenv(fakeJailerEnv, filepath.Join(dir, "argv"))
	t.Setenv(fakeJailerLoosenEnv, "vmlinux")

	bin := filepath.Join(dir, "firecracker")
	must(t, os.WriteFile(bin, []byte("fc"), 0o755))

	kernel := filepath.Join(dir, "vmlinux")
	must(t, os.WriteFile(kernel, []byte("kernel"), 0o644))

	s := LaunchSpec{Binary: bin, Dir: dir, ID: "instance", Jail: &JailSpec{
		Jailer: os.Args[0], UID: os.Getuid(), GID: os.Getgid(),
		Files: []Stage{{Name: "vmlinux", Host: kernel, Shared: true}},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pid, err := ExecLauncher{}.Launch(ctx, s)
	if err == nil {
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	}

	st, serr := os.Stat(kernel)
	must(t, serr)

	if m := st.Mode().Perm(); m&0o022 != 0 {
		t.Fatalf("after the launch (err %v) the host's shared kernel is %v: writable by the jail", err, m)
	}
}

// A kernel given as a symlink (SBX_FC_KERNEL=/boot/vmlinux -> vmlinux-6.1) is linked as the file
// it names: link(2) on Linux links the symlink itself, which in the chroot names nothing.
func TestASharedSymlinkIsStagedAsTheFileItNames(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "vm")
	must(t, os.MkdirAll(dir, 0o700))

	real := filepath.Join(base, "vmlinux-6.1")
	must(t, os.WriteFile(real, []byte("kernel"), 0o644))

	link := filepath.Join(base, "vmlinux")
	must(t, os.Symlink("vmlinux-6.1", link))

	root, err := PrepareJail(LaunchSpec{Binary: filepath.Join(base, "firecracker"), Dir: dir, Jail: &JailSpec{
		Jailer: "/nonexistent/jailer", UID: os.Getuid(), GID: os.Getgid(),
		Files: []Stage{{Name: "vmlinux", Host: link, Shared: true}},
	}})
	must(t, err)

	st, err := os.Lstat(filepath.Join(root, "vmlinux"))
	must(t, err)

	want, err := os.Stat(real)
	must(t, err)

	if !st.Mode().IsRegular() || !os.SameFile(st, want) {
		t.Fatalf("the jail's vmlinux is %v, not the kernel the symlink names", st.Mode())
	}
}
