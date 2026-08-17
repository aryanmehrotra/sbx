//go:build !darwin && !linux

package hostinfo

// Everywhere else the core count still means something and the memory figures do not have a
// portable answer. Left at zero, which the dashboard reads as "not known here" and does not
// draw - a made-up number is worse than a missing one for somebody deciding what to stop.
func memory() (total, free uint64) { return 0, 0 }
