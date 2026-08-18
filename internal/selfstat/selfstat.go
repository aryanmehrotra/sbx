// Package selfstat reads the resource usage of the container this process is in, from its own
// cgroup - no container runtime and no socket required.
//
// It exists for front mode. `sbx serve --front` runs beside a workload on a platform that gives
// one HTTP port and no docker socket, so there is no provider to ask "how much is this using".
// But a process can always read its own cgroup, and in a one-container-per-service deployment
// that cgroup *is* the service - so the dashboard's CPU and MEMORY columns can be filled from
// the inside, for a deployment that has no other way to report them.
//
// Everything here is a plain file read under /sys/fs/cgroup and /proc, which exist on Linux and
// not elsewhere; off Linux, and anywhere the files are missing, Read reports ok=false and the
// caller shows what it showed before - nothing, rather than a zero it did not measure.
package selfstat

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Sample is one reading of the container's own usage and ceilings.
//
// CPU is cumulative, like docker's own stats, because a rate is the difference between two of
// these over the time between them - and the dashboard already computes exactly that from the
// counters a remote deployment sends. SystemNanos is the host's total CPU time over the same
// span, so the ratio is the share of the machine.
type Sample struct {
	CPUNanos    uint64 // cumulative CPU time this container has used
	SystemNanos uint64 // cumulative CPU time across the whole host, same clock
	OnlineCPUs  int

	MemBytes uint64 // resident now
	MemLimit uint64 // the cgroup's ceiling, 0 if unlimited

	NanoCPULimit int64 // cpu.max as nanoCPUs, 0 if unlimited
}

// root is the filesystem prefix, "/" in production and a fixture dir in tests.
var root = "/"

// Read samples this container's cgroup. ok=false where the files are not there - off Linux, or a
// host that does not expose them - which the caller treats as "no reading", not as zero.
func Read() (Sample, bool) { return readFrom(root) }

func readFrom(root string) (Sample, bool) {
	var s Sample

	// cgroup v2 is one unified hierarchy, marked by cgroup.controllers at its root. v1 splits
	// the same numbers across per-controller directories, so which files to read depends on it.
	v2 := exists(filepath.Join(root, "sys/fs/cgroup/cgroup.controllers"))

	cpu, cpuOK := readCPU(root, v2)
	mem, memLimit, memOK := readMem(root, v2)

	if !cpuOK && !memOK {
		return Sample{}, false
	}

	s.CPUNanos = cpu
	s.MemBytes, s.MemLimit = mem, memLimit
	s.NanoCPULimit = readCPULimit(root, v2)
	s.OnlineCPUs = runtime.NumCPU()
	s.SystemNanos = readSystemNanos(root)

	// A CPU rate needs both halves. Without the host's total there is nothing to divide by, so
	// rather than report a container figure the dashboard cannot turn into a percentage, say the
	// CPU half is absent and let memory stand on its own.
	if s.SystemNanos == 0 {
		s.CPUNanos = 0
	}

	return s, true
}

// readCPU returns the container's cumulative CPU time in nanoseconds.
func readCPU(root string, v2 bool) (uint64, bool) {
	if v2 {
		// cpu.stat: "usage_usec N" is microseconds since the cgroup was created.
		for _, line := range lines(root, "sys/fs/cgroup/cpu.stat") {
			if usec, ok := field(line, "usage_usec"); ok {
				return usec * 1000, true
			}
		}

		return 0, false
	}

	// v1 records nanoseconds directly, under one of two layouts.
	for _, p := range []string{
		"sys/fs/cgroup/cpuacct/cpuacct.usage",
		"sys/fs/cgroup/cpu,cpuacct/cpuacct.usage",
	} {
		if n, ok := readUint(root, p); ok {
			return n, true
		}
	}

	return 0, false
}

// readMem returns resident bytes now and the cgroup's memory ceiling.
func readMem(root string, v2 bool) (used, limit uint64, ok bool) {
	if v2 {
		used, ok = readUint(root, "sys/fs/cgroup/memory.current")
		limit, _ = readUint(root, "sys/fs/cgroup/memory.max") // "max" parses as 0 = unlimited

		return used, limit, ok
	}

	used, ok = readUint(root, "sys/fs/cgroup/memory/memory.usage_in_bytes")
	limit, _ = readUint(root, "sys/fs/cgroup/memory/memory.limit_in_bytes")

	// v1 spells "unlimited" as a number so large it is really the absence of a limit; anything
	// at or past the whole address space is not a ceiling worth drawing a bar against.
	if limit >= 1<<62 {
		limit = 0
	}

	return used, limit, ok
}

// readCPULimit turns a cgroup CPU quota into nanoCPUs, matching provider.Limits.NanoCPUs.
func readCPULimit(root string, v2 bool) int64 {
	if v2 {
		// cpu.max: "<quota> <period>" in microseconds, or "max <period>" for unlimited.
		f := strings.Fields(first(root, "sys/fs/cgroup/cpu.max"))
		if len(f) == 2 && f[0] != "max" {
			quota, _ := strconv.ParseInt(f[0], 10, 64)
			period, _ := strconv.ParseInt(f[1], 10, 64)

			if quota > 0 && period > 0 {
				return quota * 1_000_000_000 / period
			}
		}

		return 0
	}

	quota, _ := readInt(root, "sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	period, _ := readInt(root, "sys/fs/cgroup/cpu/cpu.cfs_period_us")

	if quota > 0 && period > 0 {
		return quota * 1_000_000_000 / period
	}

	return 0
}

// readSystemNanos is the host's total CPU time from /proc/stat, on the same clock as the
// container counter. The first "cpu" line sums every jiffy across every core; USER_HZ is 100 on
// Linux, so each jiffy is 10 ms. This is the whole machine, which is the point - the ratio of
// the two deltas is a share of it.
func readSystemNanos(root string) uint64 {
	for _, line := range lines(root, "proc/stat") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}

		var jiffies uint64

		for _, f := range strings.Fields(line)[1:] {
			n, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return 0
			}

			jiffies += n
		}

		return jiffies * 10_000_000 // 10 ms per jiffy, in nanoseconds
	}

	return 0
}

// --- small file helpers, each tolerant of a missing file --------------------------------------

func exists(p string) bool {
	_, err := os.Stat(p)

	return err == nil
}

func first(root, rel string) string {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(b))
}

func lines(root, rel string) []string {
	return strings.Split(first(root, rel), "\n")
}

func readUint(root, rel string) (uint64, bool) {
	s := first(root, rel)

	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}

	return n, true
}

func readInt(root, rel string) (int64, bool) {
	n, err := strconv.ParseInt(first(root, rel), 10, 64)

	return n, err == nil
}

// field pulls the value from a "key value" line, if the key matches.
func field(line, key string) (uint64, bool) {
	f := strings.Fields(line)
	if len(f) != 2 || f[0] != key {
		return 0, false
	}

	n, err := strconv.ParseUint(f[1], 10, 64)

	return n, err == nil
}
