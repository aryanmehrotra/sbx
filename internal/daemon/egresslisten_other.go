//go:build !linux

package daemon

import "net"

// listenFree is a plain bind off Linux: an sbx-owned bridge (a microVM's) exists only on Linux,
// so there is no address here that could appear later.
func listenFree(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
