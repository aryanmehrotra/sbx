package fc

import (
	"fmt"
	"net"
	"syscall"
)

// peerIs checks that the process listening at the other end of c runs as uid: the kernel's
// SO_PEERCRED, taken at its listen(), which no path swap can change.
func peerIs(c net.Conn, uid int) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("not a unix connection")
	}

	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}

	var (
		cred *syscall.Ucred
		gerr error
	)

	if err := raw.Control(func(fd uintptr) {
		cred, gerr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}

	if gerr != nil {
		return gerr
	}

	if int(cred.Uid) != uid {
		return fmt.Errorf("answered by uid %d (pid %d), not the jail's %d", cred.Uid, cred.Pid, uid)
	}

	return nil
}
