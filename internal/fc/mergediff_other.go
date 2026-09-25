//go:build !linux

package fc

import "errors"

func mergeDiff(string, string) error {
	return errors.New("merging a Diff snapshot uses Linux's SEEK_DATA; firecracker runs only on Linux")
}
