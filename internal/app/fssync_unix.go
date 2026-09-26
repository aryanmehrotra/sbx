//go:build linux || darwin

package app

import "syscall"

// fssync is sync(2) with no dependency on the image: the firecracker provider runs it in a VM
// through execd before copying the VM's disk for a snapshot, so that what a caller wrote a moment
// earlier - still in the guest's page cache - is on the disk being copied. An image with no
// `sync` (distroless) still has this binary, at /opt/sbx/sbx.
func fssync([]string) int {
	syscall.Sync()
	return 0
}
