package provider

import "os"

// allocated is the file's length: the provider never runs on Windows, whose helper VM holds the
// real files, so this only has to compile.
func allocated(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}

	return fi.Size()
}

// links is 1: nothing is jailed on Windows.
func links(string) uint64 { return 1 }
