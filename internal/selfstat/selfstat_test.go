package selfstat

import (
	"os"
	"path/filepath"
	"testing"
)

// write lays out fixture files under a temp root, so the reader can be tested off Linux and
// without a real cgroup - the files are the whole interface it depends on.
func write(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

// A cgroup v2 container: usage from cpu.stat in microseconds, memory.current in bytes, and a
// cpu.max quota that becomes nanoCPUs.
func TestReadsCgroupV2(t *testing.T) {
	root := write(t, map[string]string{
		"sys/fs/cgroup/cgroup.controllers": "cpu memory",
		"sys/fs/cgroup/cpu.stat":           "usage_usec 1500000\nuser_usec 1000000\nsystem_usec 500000\n",
		"sys/fs/cgroup/cpu.max":            "50000 100000\n",
		"sys/fs/cgroup/memory.current":     "314572800\n",
		"sys/fs/cgroup/memory.max":         "1073741824\n",
		"proc/stat":                        "cpu 100 0 100 800 0 0 0 0 0 0\ncpu0 100 0 100 800 0 0 0 0 0 0\n",
	})

	s, ok := readFrom(root)
	if !ok {
		t.Fatal("v2 cgroup was not read")
	}

	if s.CPUNanos != 1_500_000_000 {
		t.Errorf("CPU: got %d ns, want 1.5e9 (1.5e6 usec)", s.CPUNanos)
	}

	if s.MemBytes != 300<<20 {
		t.Errorf("memory: got %d, want %d", s.MemBytes, 300<<20)
	}

	if s.MemLimit != 1<<30 {
		t.Errorf("memory limit: got %d, want 1GiB", s.MemLimit)
	}

	// 50000/100000 of a core = half a core = 0.5e9 nanoCPUs.
	if s.NanoCPULimit != 500_000_000 {
		t.Errorf("cpu limit: got %d, want 5e8", s.NanoCPULimit)
	}

	// /proc/stat total = 1000 jiffies * 10ms = 10e9 ns.
	if s.SystemNanos != 10_000_000_000 {
		t.Errorf("system: got %d, want 1e10", s.SystemNanos)
	}
}

// A cgroup v1 container: CPU in nanoseconds directly, memory usage_in_bytes, and the sentinel
// "unlimited" memory limit that must read as no ceiling rather than a colossal one.
func TestReadsCgroupV1(t *testing.T) {
	root := write(t, map[string]string{
		"sys/fs/cgroup/cpuacct/cpuacct.usage":        "2000000000\n",
		"sys/fs/cgroup/memory/memory.usage_in_bytes": "104857600\n",
		"sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n", // ~unlimited
		"sys/fs/cgroup/cpu/cpu.cfs_quota_us":         "-1\n",
		"sys/fs/cgroup/cpu/cpu.cfs_period_us":        "100000\n",
		"proc/stat":                                  "cpu 200 0 200 600 0 0 0 0 0 0\n",
	})

	s, ok := readFrom(root)
	if !ok {
		t.Fatal("v1 cgroup was not read")
	}

	if s.CPUNanos != 2_000_000_000 {
		t.Errorf("CPU: got %d, want 2e9", s.CPUNanos)
	}

	if s.MemBytes != 100<<20 {
		t.Errorf("memory: got %d, want %d", s.MemBytes, 100<<20)
	}

	if s.MemLimit != 0 {
		t.Errorf("an unlimited v1 memory limit should read as 0, got %d", s.MemLimit)
	}

	if s.NanoCPULimit != 0 {
		t.Errorf("a -1 quota is unlimited, want 0, got %d", s.NanoCPULimit)
	}
}

// Where nothing is there - off Linux, or a host that hides cgroups - Read says so rather than
// reporting zero, so the dashboard shows "no reading" instead of "using nothing".
func TestAbsentCgroupIsNotZero(t *testing.T) {
	if _, ok := readFrom(t.TempDir()); ok {
		t.Error("an empty root reported a reading")
	}
}

// A CPU counter with no host total to divide it by is not a percentage anyone can draw, so the
// CPU half is dropped and memory stands alone rather than shipping an unusable number.
func TestCPUNeedsTheSystemTotal(t *testing.T) {
	root := write(t, map[string]string{
		"sys/fs/cgroup/cgroup.controllers": "cpu memory",
		"sys/fs/cgroup/cpu.stat":           "usage_usec 1500000\n",
		"sys/fs/cgroup/memory.current":     "314572800\n",
		// no proc/stat
	})

	s, ok := readFrom(root)
	if !ok {
		t.Fatal("should still read memory")
	}

	if s.CPUNanos != 0 {
		t.Errorf("CPU should be dropped without a system total, got %d", s.CPUNanos)
	}

	if s.MemBytes != 300<<20 {
		t.Errorf("memory should still be read, got %d", s.MemBytes)
	}
}
