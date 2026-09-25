//go:build linux

package execd

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func withRoots(t *testing.T, cgroup, proc string) {
	t.Helper()

	oldC, oldP := cgroupRoot, procRoot
	cgroupRoot, procRoot = cgroup, proc

	t.Cleanup(func() { cgroupRoot, procRoot = oldC, oldP })
}

func TestCgroupMetrics(t *testing.T) {
	proc := fixture(t, map[string]string{
		"meminfo": "MemTotal:       8388608 kB\nMemFree: 1 kB\nMemAvailable:   4194304 kB\n",
		"stat":    "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 1 1 1 1\n",
	})

	cases := []struct {
		name        string
		cgroup      map[string]string
		busy        time.Duration
		total, used float64
	}{
		{
			name: "v2 limited",
			cgroup: map[string]string{
				"cpu.stat": "usage_usec 2500000\nuser_usec 1\n", "memory.current": "314572800",
				"memory.max": "1073741824\n", "memory.stat": "anon 1\ninactive_file 104857600\n",
			},
			busy: 2500 * time.Millisecond, total: 1024, used: 200,
		},
		{
			name:   "v2 unlimited falls back to the host total",
			cgroup: map[string]string{"cpu.stat": "usage_usec 1\n", "memory.current": "1048576", "memory.max": "max\n"},
			busy:   time.Microsecond, total: 8192, used: 1,
		},
		{
			name: "v1",
			cgroup: map[string]string{
				"cpuacct/cpuacct.usage": "3000000000\n", "memory/memory.usage_in_bytes": "209715200",
				"memory/memory.limit_in_bytes": "9223372036854771712", "memory/memory.stat": "total_inactive_file 0\n",
			},
			busy: 3 * time.Second, total: 8192, used: 200,
		},
		{
			name:   "no cgroup reads /proc",
			cgroup: map[string]string{},
			busy:   2 * time.Second, total: 8192, used: 4096,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withRoots(t, fixture(t, c.cgroup), proc)

			r := readCPU()
			if r == nil || r.busy != c.busy {
				t.Fatalf("cpu reading %+v, want busy %s", r, c.busy)
			}

			total, used := readMemory()
			if total != c.total || used != c.used {
				t.Fatalf("memory %v/%v MiB, want %v/%v", used, total, c.used, c.total)
			}
		})
	}
}
