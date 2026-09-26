package fc

import "strings"

// parseCgroup2Mount is the mount point of the first cgroup2 filesystem in a /proc/mounts text.
func parseCgroup2Mount(mounts string) string {
	for _, line := range strings.Split(mounts, "\n") {
		if f := strings.Fields(line); len(f) >= 3 && f[2] == "cgroup2" {
			return f[1]
		}
	}

	return ""
}

// Cgroup2 is where the unified cgroup hierarchy is mounted on this host, if it is.
func Cgroup2() (string, bool) {
	mnt := cgroup2Mount()
	return mnt, mnt != ""
}
