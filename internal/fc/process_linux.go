package fc

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
)

// detached puts firecracker in its own session, so a Ctrl-C on the `sbx create` that started
// it - delivered to the whole foreground process group - does not take the VM down with it.
func detached() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// ownsPID reports whether pid is a live process whose command line names sock. /proc is the
// only place that answers "is this still MY firecracker" without a pidfd held from birth,
// which a VM started by another sbx process never gave us.
func ownsPID(pid int, sock string) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return false
	}

	return bytes.Contains(b, []byte(sock))
}

func killPID(pid int) error {
	err := syscall.Kill(pid, syscall.SIGKILL)
	if err == syscall.ESRCH {
		return nil
	}

	return err
}
