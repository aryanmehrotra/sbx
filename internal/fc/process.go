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
	ID     string // --id, shown in firecracker's own logs
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
}

// ExecLauncher runs the real binary.
type ExecLauncher struct{}

// Launch starts firecracker with its stdout (the guest's serial console) appended to
// console.log and its own log on stderr in vmm.log. Appended, so the console of a VM that has
// slept and woken ten times reads as one history, which is what `sbx logs` promises.
func (ExecLauncher) Launch(ctx context.Context, s LaunchSpec) (int, error) {
	sock := filepath.Join(s.Dir, APISockName)

	// firecracker refuses to bind a path that exists, and a killed one leaves its socket.
	if err := os.Remove(sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}

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

	// Not CommandContext: ctx bounds the launch, not the VM's life.
	cmd := exec.Command(s.Binary, "--api-sock", sock, "--id", s.ID)
	cmd.Dir = s.Dir
	cmd.Stdout = console
	cmd.Stderr = vmmLog
	cmd.SysProcAttr = detached()

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting %s: %w", s.Binary, err)
	}

	pid := cmd.Process.Pid

	// Reap it if it dies while we are still here; if we exit first, init does. Without this a
	// long-lived `sbx serve` would collect a zombie per VM it ever stopped.
	go func() { _ = cmd.Wait() }()

	if err := os.WriteFile(filepath.Join(s.Dir, PIDName), fmt.Appendf(nil, "%d\n", pid), 0o600); err != nil {
		_ = cmd.Process.Kill()
		return 0, err
	}

	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := NewClient(sock).WaitReady(wctx); err != nil {
		_ = cmd.Process.Kill()

		return 0, fmt.Errorf("%w - firecracker's own log is %s", err, filepath.Join(s.Dir, VMMLogName))
	}

	return pid, nil
}

// Kill sends SIGKILL to the recorded PID, but only after checking that PID is still a
// firecracker serving THIS directory's socket. PIDs are reused, and a daemon that slept a VM
// yesterday must not kill whatever inherited its number today.
func (ExecLauncher) Kill(ctx context.Context, dir string) error {
	pid, err := ReadPID(dir)
	if err != nil {
		return nil // never started, or already cleaned up
	}

	if !ownsPID(pid, filepath.Join(dir, APISockName)) {
		_ = os.Remove(filepath.Join(dir, PIDName))
		return nil
	}

	if err := killPID(pid); err != nil {
		return fmt.Errorf("killing firecracker pid %d: %w", pid, err)
	}

	// Wait for it to be gone, so the next Launch's socket and rootfs are not still held.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if !ownsPID(pid, filepath.Join(dir, APISockName)) {
			_ = os.Remove(filepath.Join(dir, PIDName))
			_ = os.Remove(filepath.Join(dir, APISockName))

			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}

	return fmt.Errorf("firecracker pid %d did not exit after SIGKILL; it is stuck in the kernel "+
		"(check `cat /proc/%d/stack` as root)", pid, pid)
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
