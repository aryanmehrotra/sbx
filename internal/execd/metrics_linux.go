//go:build linux

package execd

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"time"
)

// cgroupRoot is where a container sees its own cgroup (docker and podman mount a private
// cgroup namespace there). A variable so tests can point it at a fixture tree.
var (
	cgroupRoot = "/sys/fs/cgroup"
	procRoot   = "/proc"
)

// readCPU prefers the cgroup's own usage counter - the sandbox's CPU, not the host's - and
// falls back to /proc/stat when there is no cgroup to read.
func readCPU() *cpuReading {
	now := time.Now()

	// cgroup v2
	if f, err := os.Open(cgroupRoot + "/cpu.stat"); err == nil {
		defer f.Close()

		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "usage_usec "); ok {
				if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
					return &cpuReading{busy: time.Duration(n) * time.Microsecond, at: now}
				}
			}
		}
	}

	// cgroup v1
	for _, p := range []string{"/cpuacct/cpuacct.usage", "/cpu,cpuacct/cpuacct.usage"} {
		if n, ok := readInt(cgroupRoot + p); ok {
			return &cpuReading{busy: time.Duration(n), at: now}
		}
	}

	return procStatCPU(now)
}

// procStatCPU sums the busy columns of /proc/stat's aggregate line. The kernel reports them in
// USER_HZ, which is 100 on every Linux ABI Go supports.
func procStatCPU(now time.Time) *cpuReading {
	f, err := os.Open(procRoot + "/stat")
	if err != nil {
		return nil
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return nil
	}

	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return nil
	}

	var busy int64

	for i, v := range fields[1:] {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil
		}

		// Columns 4 and 5 are idle and iowait; guest time is already inside user.
		if i == 3 || i == 4 || i >= 8 {
			continue
		}

		busy += n
	}

	return &cpuReading{busy: time.Duration(busy) * 10 * time.Millisecond, at: now}
}

// readMemory returns total and used MiB: the cgroup's limit and usage when it has a limit, the
// machine's otherwise. Used excludes inactive page cache, as `docker stats` does, so a sandbox
// that once read a big file does not look full forever.
func readMemory() (total, used float64) {
	hostTotal, hostUsed := meminfo()

	limit, usage, ok := cgroupMemory()
	if !ok {
		return mib(hostTotal), mib(hostUsed)
	}

	if limit <= 0 || (hostTotal > 0 && limit > hostTotal) {
		limit = hostTotal
	}

	return mib(limit), mib(usage)
}

func cgroupMemory() (limit, usage int64, ok bool) {
	// cgroup v2
	if cur, found := readInt(cgroupRoot + "/memory.current"); found {
		if raw, err := os.ReadFile(cgroupRoot + "/memory.max"); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64); err == nil {
				limit = n // "max" leaves it 0: unlimited
			}
		}

		return limit, cur - statField(cgroupRoot+"/memory.stat", "inactive_file"), true
	}

	// cgroup v1
	if cur, found := readInt(cgroupRoot + "/memory/memory.usage_in_bytes"); found {
		if n, found := readInt(cgroupRoot + "/memory/memory.limit_in_bytes"); found && n < 1<<60 {
			limit = n // v1 spells "unlimited" as a number near 2^63
		}

		return limit, cur - statField(cgroupRoot+"/memory/memory.stat", "total_inactive_file"), true
	}

	return 0, 0, false
}

func meminfo() (total, used int64) {
	f, err := os.Open(procRoot + "/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()

	var avail int64

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}

		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}

		switch fields[0] {
		case "MemTotal:":
			total = n << 10
		case "MemAvailable:":
			avail = n << 10
		}
	}

	return total, total - avail
}

func readInt(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}

	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)

	return n, err == nil
}

func statField(path, key string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), key+" "); ok {
			n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			return n
		}
	}

	return 0
}

func mib(b int64) float64 {
	return float64(max(b, 0)) / (1 << 20)
}
