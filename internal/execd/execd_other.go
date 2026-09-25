//go:build !unix

package execd

import (
	"fmt"
	"os"
)

// Main refuses on platforms without process groups and POSIX signals. execd only ever runs
// inside a Linux container; it builds here so `sbx` itself still builds for every target.
func Main(args []string) int {
	fmt.Fprintln(os.Stderr, "sbx execd: runs only on Linux (inside a sandbox) or another unix; "+
		"there is nothing to run it for on this platform")

	return 2
}
