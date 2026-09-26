//go:build !(linux || darwin)

package app

import (
	"fmt"
	"os"
	"runtime"
)

// fssync runs inside a Linux guest; there is no sandbox of this kind on other platforms.
func fssync([]string) int {
	fmt.Fprintln(os.Stderr, "sbx fssync: runs inside a Linux sandbox, not on "+runtime.GOOS)
	return 1
}
