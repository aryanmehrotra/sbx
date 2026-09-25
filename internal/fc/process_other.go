//go:build !linux

package fc

import (
	"errors"
	"syscall"
)

// Firecracker is a Linux program; on any other OS the provider is never selected (hostcap says
// helper-vm or refused), so these only have to compile and say so if somehow reached.
func detached() *syscall.SysProcAttr { return nil }

func ownsPID(int, string) bool { return false }

func killPID(int) error { return errors.New("firecracker runs only on Linux") }
