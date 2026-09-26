// Package guestinit is PID 1 inside a Firecracker VM: `sbx fc-init`.
//
// The kernel boots the per-VM agent drive as its root and runs /sbx with "fc-init". This mounts
// the image's root filesystem, makes it look like a container's (proc, sys, dev, a devpts, the
// agent at /opt/sbx/sbx), switches into it, and execs execd as the same PID 1 with the image's
// entrypoint as its child - so from execd onward nothing can tell a VM from a container, which
// is the point: execd, its API and the conformance suite do not change.
//
// It does as little as a container runtime would and no more. /etc/hostname and /etc/hosts are
// filled as docker's bind mounts would have them; no /etc/resolv.conf (there is no egress to
// resolve for), no users, no cgroups: a VM is its own cgroup.
package guestinit

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/fc"
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

// writeHostname fills /etc/hostname the way docker's bind mount would. `docker export` of an
// image's container gives an empty file there (docker mounts over it), so without this the
// workload reads no name from it. Best effort: a read-only root keeps the empty file.
func writeHostname(path, name string) {
	if name == "" {
		return
	}

	_ = os.WriteFile(path, []byte(name+"\n"), 0o644)
}

// writeHosts gives /etc/hosts what docker's would have: loopback names and the hostname.
// `docker export` leaves an empty file there (docker mounts over it), and a VM has no DNS, so
// without this "localhost" resolves to nothing. Lines the image already has are kept; best effort,
// like writeHostname.
func writeHosts(path, name string) {
	b, _ := os.ReadFile(path)
	have := string(b)

	var add strings.Builder

	if !strings.Contains(have, "localhost") {
		add.WriteString("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	}

	if name != "" && !strings.Contains(have, "\t"+name+"\n") && !strings.Contains(have, " "+name+"\n") {
		add.WriteString("127.0.1.1\t" + name + "\n")
	}

	if add.Len() == 0 {
		return
	}

	if have != "" && !strings.HasSuffix(have, "\n") {
		have += "\n"
	}

	_ = os.WriteFile(path, []byte(have+add.String()), 0o644)
}

// mountStep is one mount fc-init makes for an extra drive; planned here, where it can be tested
// on any OS, and carried out by the Linux half.
type mountStep struct {
	MkdirAll string // created first, when set

	Source, Target, FSType string
	Bind, ReadOnly         bool

	// Remount makes an existing bind read-only: a bind takes no flags of its own on creation.
	Remount bool
}

// drivePlan is how each extra drive reaches the workload: mounted at its target inside root and,
// with a sub_path, that directory bound over the target so the rest of the drive is not
// reachable there. A read-only drive is mounted read-only, and so is its bind.
func drivePlan(root string, ms []fc.InitMount) []mountStep {
	var out []mountStep

	for _, m := range ms {
		target := filepath.Join(root, filepath.Clean("/"+m.Target))

		out = append(out, mountStep{MkdirAll: target, Source: m.Device, Target: target, FSType: "ext4",
			ReadOnly: m.ReadOnly})

		if m.SubPath == "" {
			continue
		}

		sub := filepath.Join(target, filepath.Clean("/"+m.SubPath))

		step := mountStep{Source: sub, Target: target, Bind: true}
		if !m.ReadOnly {
			step.MkdirAll = sub // docker creates a missing sub_path in a volume, and so does this
		}

		out = append(out, step)

		if m.ReadOnly {
			out = append(out, mountStep{Source: sub, Target: target, Bind: true, Remount: true, ReadOnly: true})
		}
	}

	return out
}
