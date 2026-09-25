//go:build !linux

package fc

import (
	"errors"
	"os"
)

// reflink is Linux's FICLONE. Elsewhere the provider never runs a VM, and CloneFile's sparse
// copy is what the tests exercise.
func reflink(*os.File, *os.File) error { return errors.New("reflink: linux only") }
