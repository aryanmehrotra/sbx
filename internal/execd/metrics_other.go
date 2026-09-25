//go:build unix && !linux

package execd

// Outside Linux there is no /proc or cgroup to read. execd only runs in Linux sandboxes; these
// report zero so the endpoints still answer, with the right shape, in unit tests on a Mac.
func readCPU() *cpuReading { return nil }

func readMemory() (total, used float64) { return 0, 0 }
