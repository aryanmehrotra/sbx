//go:build unix && !linux

package execd

import (
	"os"
	"time"
)

// createdAt falls back to the modification time where the stat layout differs; execd runs on
// Linux, and these platforms only build it for the unit tests.
func createdAt(fi os.FileInfo) time.Time {
	return fi.ModTime()
}
