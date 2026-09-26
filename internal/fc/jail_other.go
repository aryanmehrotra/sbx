//go:build !unix

package fc

import "os"

// linkCount is 1 where the platform cannot say: the jailer runs only on Linux, so nothing here
// ever adopts a file from one.
func linkCount(os.FileInfo) uint64 { return 1 }

// fileOwner cannot be read here; nothing here is ever jailed.
func fileOwner(os.FileInfo) (int, bool) { return 0, false }
