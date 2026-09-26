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
