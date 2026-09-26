//go:build !linux && !darwin

package fc

import "os"

// extentCopy needs SEEK_DATA/SEEK_HOLE; elsewhere CloneFile reads everything.
func extentCopy(*os.File, *os.File) error { return errNoExtents }
