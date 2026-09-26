package fc

import "testing"

func TestCgroup2MountIsTheFirstCgroup2Line(t *testing.T) {
	mounts := "proc /proc proc rw 0 0\ncgroup /sys/fs/cgroup/cpu cgroup rw,cpu 0 0\n" +
		"cgroup2 /sys/fs/cgroup/unified cgroup2 rw,nsdelegate 0 0\ncgroup2 /other cgroup2 rw 0 0\n"

	if got := parseCgroup2Mount(mounts); got != "/sys/fs/cgroup/unified" {
		t.Fatalf("got %q", got)
	}

	if got := parseCgroup2Mount("proc /proc proc rw 0 0\n"); got != "" {
		t.Fatalf("no cgroup2: %q", got)
	}
}
