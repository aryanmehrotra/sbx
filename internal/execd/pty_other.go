//go:build unix && !linux

package execd

import (
	"errors"
	"os"
	"syscall"
)

// ptySupported is false off Linux: execd only runs inside a Linux sandbox, and the pty ioctls
// are Linux's. The routes answer 501 rather than 404 so a client learns the endpoint exists.
const ptySupported = false

var errNoPTY = errors.New("pty sessions need Linux; this sbx execd is not running on Linux")

func openPTY() (master, slave *os.File, err error) { return nil, nil, errNoPTY }

func setWinsize(*os.File, uint16, uint16) error { return errNoPTY }

func ptySysProcAttr() *syscall.SysProcAttr { return nil }
