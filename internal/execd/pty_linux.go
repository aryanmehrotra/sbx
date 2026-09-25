//go:build linux

package execd

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// ptySupported: Linux is where execd runs, inside the sandbox, and the one platform whose
// pseudo-terminal ioctls are implemented here.
const ptySupported = true

// openPTY allocates a pseudo-terminal the way openpty(3) does, with the three ioctls it is made
// of rather than cgo or a module: open the multiplexer, unlock the pair, ask for its number.
//
// The master is opened with os.OpenFile and configured through SyscallConn rather than Fd, so it
// stays in non-blocking mode under the runtime poller: that is what lets Close interrupt a Read
// blocked on it, which is how a session is torn down while nothing is being printed.
func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w; the sandbox needs a devpts mount (docker provides one)", err)
	}

	var n uint32

	if err := ptyIoctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(new(int32))); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}

	if err := ptyIoctl(master, syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("get pty number: %w", err)
	}

	name := "/dev/pts/" + strconv.FormatUint(uint64(n), 10)

	slave, err = os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open %s: %w", name, err)
	}

	return master, slave, nil
}

// setWinsize sets the terminal size, which also delivers SIGWINCH to the foreground group.
func setWinsize(master *os.File, cols, rows uint16) error {
	ws := struct{ Row, Col, X, Y uint16 }{Row: rows, Col: cols}

	return ptyIoctl(master, syscall.TIOCSWINSZ, unsafe.Pointer(&ws))
}

// getWinsize reads the terminal size back; the tests use it to check a resize landed.
func getWinsize(master *os.File) (cols, rows uint16, err error) {
	var ws struct{ Row, Col, X, Y uint16 }

	err = ptyIoctl(master, syscall.TIOCGWINSZ, unsafe.Pointer(&ws))

	return ws.Col, ws.Row, err
}

func ptyIoctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}

	var errno syscall.Errno

	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	}); err != nil {
		return err
	}

	if errno != 0 {
		return errno
	}

	return nil
}

// ptySysProcAttr makes the child a session leader with the slave (its stdin) as controlling
// terminal, so ^C on the terminal reaches the foreground job and job control works.
func ptySysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
}
