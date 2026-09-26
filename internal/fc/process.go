package fc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// A VM's files, all inside its own directory. Fixed names rather than configurable ones,
// because a firecracker process restored from a snapshot is a different process than the one
// that made it, possibly started by a different sbx process, and the only thing they can agree
// on without talking is the directory.
const (
	APISockName  = "api.sock"   // the control socket firecracker binds
	VsockName    = "vsock.sock" // the hybrid-vsock host socket
	ConsoleName  = "console.log"
	VMMLogName   = "vmm.log"
	PIDName      = "firecracker.pid"
	RootfsName   = "rootfs.ext4"
	StateName    = "vm.state"
	MemName      = "vm.mem"
	DiffMemName  = "diff.mem"
	LockFileName = "lock"
)

// LaunchSpec is one firecracker process to start.
type LaunchSpec struct {
	Binary string // the firecracker executable
	Dir    string // the VM's directory; the API socket and logs go here
	ID     string // --id, shown in firecracker's own logs; a jailed VMM's is JailID(Dir)

	// Jail, when set, starts the VMM through Firecracker's jailer (jail.go): chrooted into the
	// VM's jail, as the spec's uid, in a cgroup of its own. Nil is the VMM as a plain child of
	// this process - SBX_FC_JAILER=off.
	Jail *JailSpec
}

// Launcher starts and ends firecracker processes. An interface so the provider's state
// machine is tested against fcfake, and so the helper-VM layer could supply one that launches
// somewhere else - nothing above this line assumes the VMM is a child of this process.
type Launcher interface {
	// Launch starts firecracker detached from the caller - a VM created by `sbx create` must
	// outlive that command - and returns its PID once the API socket answers.
	Launch(ctx context.Context, s LaunchSpec) (int, error)

	// Kill ends the process recorded for dir, if it is still the one that was started there.
	// A process already gone is success: the caller wanted it gone.
	Kill(ctx context.Context, dir string) error

	// Alive reports whether the process recorded for dir is still the firecracker started there.
	// It is what tells a VMM that is slow to answer its API from one that is gone.
	Alive(dir string) bool
}

// ExecLauncher runs the real binary.
type ExecLauncher struct{}

// Launch starts firecracker with its stdout (the guest's serial console) appended to
// console.log and its own log on stderr in vmm.log. Appended, so the console of a VM that has
// slept and woken ten times reads as one history, which is what `sbx logs` promises - bounded by
// CapLogs, here and on the daemon's reconcile, so that history cannot fill the disk.
func (ExecLauncher) Launch(ctx context.Context, s LaunchSpec) (int, error) {
	sock := filepath.Join(s.Dir, APISockName)

	// firecracker refuses to bind a path that exists, and a killed one leaves its socket.
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}

	// Bounded at every launch as well as on the daemon's reconcile: a wake is a launch.
	_ = CapLogs(s.Dir)

	console, err := os.OpenFile(filepath.Join(s.Dir, ConsoleName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer console.Close()

	vmmLog, err := os.OpenFile(filepath.Join(s.Dir, VMMLogName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer vmmLog.Close()

	bin, args := s.Binary, []string{"--api-sock", sock, "--id", s.ID}

	var root string

	if s.Jail != nil {
		// sock becomes a symlink into the root, where the jailed VMM binds /api.sock.
		if root, err = PrepareJail(s); err != nil {
			return 0, err
		}

		bin, args = s.Jail.Jailer, JailerArgs(s)
	}

	// Not CommandContext: ctx bounds the launch, not the VM's life.
	cmd := exec.Command(bin, args...)
	cmd.Dir = s.Dir
	cmd.Stdout = console
	cmd.Stderr = vmmLog
	cmd.SysProcAttr = detached()

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting %s: %w", bin, err)
	}

	pid := cmd.Process.Pid

	// Reap it if it dies while we are still here; if we exit first, init does. Without this a
	// long-lived `sbx serve` would collect a zombie per VM it ever stopped.
	go func() { _ = cmd.Wait() }()

	if err := os.WriteFile(filepath.Join(s.Dir, PIDName), fmt.Appendf(nil, "%d %d\n", pid, procStart(pid)), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return 0, err
	}

	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := NewClient(sock).WaitReady(wctx); err != nil {
		_ = cmd.Process.Kill()

		why := "firecracker's own log is"
		if s.Jail != nil {
			why = "the jailer's and firecracker's log is"
		}

		return 0, fmt.Errorf("%w - %s %s", err, why, filepath.Join(s.Dir, VMMLogName))
	}

	// The jailer has finished with the root once its VMM answers, and no guest has run yet: the
	// moment to hold the files every VM shares to what stage left.
	if s.Jail != nil {
		if err := secureShared(root, s.Jail.Files); err != nil {
			_ = cmd.Process.Kill()
			return 0, err
		}
	}

	return pid, nil
}

// The process table as Kill and Alive read it and the signal Kill sends: variables so what they
// decide is tested on every OS, not only where /proc is.
var (
	ownsProc    = owns
	holdingProc = holding
	killProc    = killPID
)

// Kill sends SIGKILL to the recorded PID, but only after checking that PID is still a
// firecracker serving THIS directory's socket. PIDs are reused, and a daemon that slept a VM
// yesterday must not kill whatever inherited its number today.
func (ExecLauncher) Kill(ctx context.Context, dir string) error {
	pid, start, err := readPIDFile(dir)
	if err != nil {
		releaseJail(dir)
		return nil // never started, or already cleaned up
	}

	sock := filepath.Join(dir, APISockName)

	switch {
	// Its argv names this VM, or the kernel says it is still the very process Launch started (the
	// pid AND its start time): a VMM can overwrite its own command line, never those. Waiting on
	// one that erased its argv without signalling it would wait for ever.
	case ownsProc(pid, dir) || (start != 0 && holdingProc(pid, start)):
		if err := killProc(pid); err != nil {
			return fmt.Errorf("killing firecracker pid %d: %w", pid, err)
		}
	case start == 0 || !holdingProc(pid, start):
		// Not ours any more (a reused pid, or an old pid file with no start time and a process
		// that no longer names this socket): nothing of ours to wait for.
		_ = os.Remove(filepath.Join(dir, PIDName))
		releaseJail(dir)

		return nil
	}

	// Wait until it has let go of everything, not until its command line empties: see holding.
	// A firecracker still closing its tap makes the next one's snapshot/load fail with EBUSY.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if !ownsProc(pid, dir) && !holdingProc(pid, start) {
			_ = os.Remove(filepath.Join(dir, PIDName))
			_ = os.Remove(sock)

			releaseJail(dir)

			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}

	return fmt.Errorf("firecracker pid %d did not exit within 5s of SIGKILL; it may still hold its tap "+
		"and drives (check `cat /proc/%d/stack` as root)", pid, pid)
}

// Alive is the recorded PID, checked to still be a firecracker serving this directory's socket.
func (ExecLauncher) Alive(dir string) bool {
	pid, start, err := readPIDFile(dir)
	if err != nil {
		return false
	}

	return owns(pid, dir) || (start != 0 && holding(pid, start))
}

// owns reports whether pid is the VMM of dir: an unjailed one names dir's API socket on its
// command line; a jailed one names only "/api.sock", and is known by its --id, JailID(dir), which
// the jailer passes on to the firecracker it execs.
func owns(pid int, dir string) bool {
	return ownsPID(pid, filepath.Join(dir, APISockName)) || ownsPID(pid, "\x00--id\x00"+JailID(dir)+"\x00")
}

// releaseJail removes what a jailed VMM leaves behind once it has exited: its root - whose hard
// links would otherwise keep a replaced snapshot's blocks allocated - and its cgroup, which the
// jailer never removes. Nothing to do for an unjailed one.
func releaseJail(dir string) {
	_ = os.RemoveAll(filepath.Join(dir, JailDirName))
	removeCgroup(JailCgroupParent + "/" + JailID(dir))
}

// readPIDFile is the PID file: the pid, and its start time when Launch recorded one (0 for a file
// written before it did).
func readPIDFile(dir string) (int, uint64, error) {
	pid, err := ReadPID(dir)
	if err != nil {
		return 0, 0, err
	}

	b, _ := os.ReadFile(filepath.Join(dir, PIDName))

	var p int

	var start uint64

	_, _ = fmt.Sscanf(string(b), "%d %d", &p, &start)

	return pid, start, nil
}

// ReadPID reads the PID file in dir.
func ReadPID(dir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(dir, PIDName))
	if err != nil {
		return 0, err
	}

	var pid int
	if _, err := fmt.Sscanf(string(b), "%d", &pid); err != nil || pid <= 0 {
		return 0, fmt.Errorf("unreadable pid file %s", filepath.Join(dir, PIDName))
	}

	return pid, nil
}
