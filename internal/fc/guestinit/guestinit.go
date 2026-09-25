// Package guestinit is PID 1 inside a Firecracker VM: `sbx fc-init`.
//
// The kernel boots the per-VM agent drive as its root and runs /sbx with "fc-init". This mounts
// the image's root filesystem, makes it look like a container's (proc, sys, dev, a devpts, the
// agent at /opt/sbx/sbx), switches into it, and execs execd as the same PID 1 with the image's
// entrypoint as its child - so from execd onward nothing can tell a VM from a container, which
// is the point: execd, its API and the conformance suite do not change.
//
// It does as little as a container runtime would and no more. No /etc/resolv.conf (there is no
// egress to resolve for), no users, no cgroups: a VM is its own cgroup.
package guestinit

import (
	"os"
	"strings"
)

// execdArgs is how PID 1 hands over to execd. --vsock-port is execd's AF_VSOCK listener (the
// guest agent work, branch osb/fc-vsock); the workload follows `--`.
func execdArgs(agent string, vsockPort string, argv []string) []string {
	args := []string{agent, "execd", "--vsock-port", vsockPort}
	if len(argv) > 0 {
		args = append(append(args, "--"), argv...)
	}

	return args
}

// environ is the workload's environment: the config's, with PATH and HOME defaulted the way
// docker defaults them, because an image that sets neither still expects both.
func environ(env []string) []string {
	out := append([]string{}, env...)

	has := func(k string) bool {
		for _, kv := range out {
			if strings.HasPrefix(kv, k+"=") {
				return true
			}
		}

		return false
	}

	if !has("PATH") {
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}

	if !has("HOME") {
		out = append(out, "HOME=/root")
	}

	return out
}

func fail(msg string) int {
	// The serial console is the only place this can go, and `sbx logs` reads it.
	_, _ = os.Stderr.WriteString("sbx fc-init: " + msg + "\n")
	return 1
}
