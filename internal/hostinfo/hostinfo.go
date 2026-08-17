// Package hostinfo reports what the machine a person is sitting at has.
//
// Not what the container runtime has. On macOS and Windows those are different machines: the
// containers live in a VM with its own cores and its own memory, and a Mac with 32 GB whose
// colima was given 8 is a machine where 6 GB of sandboxes is nearly full. The dashboard needs
// both figures to say anything useful - the VM's, because that is what the sandboxes contend
// for, and the laptop's, because that is what the person is deciding about.
//
// Read with the tools each system already answers this question with, and left blank where it
// cannot be answered. A guess about free memory is worse than a gap: somebody looking at this
// is deciding whether to stop something.
package hostinfo

import "runtime"

// Machine is what this host has. Zero fields mean "not known here" rather than "none".
type Machine struct {
	Cores int

	// MemBytes is the physical memory, and FreeBytes what is available to allocate now.
	//
	// Available, not unused. Every operating system worth the name spends idle memory on cache
	// it will give back the moment something asks, so "free" in its narrow sense is a number
	// that reads as alarming on a machine that is fine.
	MemBytes  uint64
	FreeBytes uint64
}

// Read reports what it can about this machine, and says nothing it cannot establish.
func Read() Machine {
	m := Machine{Cores: runtime.NumCPU()}

	m.MemBytes, m.FreeBytes = memory()

	return m
}
