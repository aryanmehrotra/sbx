//go:build !linux

package fc

import "net"

// peerIs has no SO_PEERCRED to read here; the jailer runs only on Linux, and DialVMM's Lstat and
// owner checks are what a jail-shaped directory gets.
func peerIs(net.Conn, int) error { return nil }
