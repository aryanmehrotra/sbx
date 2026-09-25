//go:build !linux

package provider

// fileLock is Linux-only because the provider only runs VMs there; elsewhere it is reached only
// by tests, which are one process.
func fileLock(string) func() { return func() {} }
