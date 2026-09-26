package fc

import "strings"

// InitConfig is what the guest's PID 1 (`sbx fc-init`) reads from /init.json on the agent drive:
// everything about the workload that a container runtime would have taken from the image config
// and the create flags, and a VM has no runtime to take it from.
//
// It is per VM, so it lives on the small per-VM agent drive rather than in the shared rootfs -
// see RootfsBuilder for why the image rootfs holds the image and nothing else.
type InitConfig struct {
	// Argv is the workload: the image's ENTRYPOINT + CMD after the spec's overrides, exactly as
	// docker would compose them. Empty means execd serves with no child.
	Argv []string `json:"argv"`

	// Env is KEY=VALUE, image first and then the spec, so the spec wins on a repeated key.
	Env []string `json:"env"`

	WorkingDir string `json:"working_dir,omitempty"`
	Hostname   string `json:"hostname,omitempty"`

	// RootDevice is the image rootfs as the guest sees it, mounted read-write and switched into.
	RootDevice string `json:"root_device"`

	// Mounts are extra drives - an OpenSandbox pvc, one ext4 image each - mounted into the image
	// root before it is switched into, in order.
	Mounts []InitMount `json:"mounts,omitempty"`
}

// InitMount is one extra drive and where the workload sees it.
type InitMount struct {
	Device string `json:"device"` // /dev/vdc, ...
	Target string `json:"target"` // absolute, inside the image root

	// SubPath mounts this directory of the drive rather than its root: the drive is mounted at
	// Target and SubPath is then bound over it, so nothing else of the drive is reachable there.
	SubPath  string `json:"sub_path,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// GuestExtraDevice is the guest device of the i-th extra drive (0-based): Firecracker attaches
// them after the agent (vda) and the rootfs (vdb), in the order they were PUT.
func GuestExtraDevice(i int) string { return "/dev/vd" + string(rune('c'+i)) }

// MaxExtraDrives keeps GuestExtraDevice within vdc..vdz.
const MaxExtraDrives = 24

// Guest device names. Firecracker attaches the root drive first and the rest in the order they
// were PUT, as virtio-blk vda, vdb, ... - so the agent drive is vda and the image is vdb.
const (
	GuestAgentDevice  = "/dev/vda"
	GuestRootfsDevice = "/dev/vdb"

	// GuestAgentPath is where the agent is visible inside the workload's root, the same path the
	// docker provider mounts it at, so nothing downstream needs to know which provider ran it.
	GuestAgentPath = "/opt/sbx/sbx"
)

// Compose is docker's rule for what a container runs: an entrypoint override replaces the
// image's ENTRYPOINT and drops its CMD; args replace CMD. Written out because a VM has nobody
// else to apply it, and getting it subtly different would run a different program than
// `--provider docker` does from the same spec.
func Compose(imageEntry, imageCmd, entry, args []string) []string {
	var e, c []string

	switch {
	case len(entry) > 0:
		e = entry
		c = args
	default:
		e = imageEntry
		c = imageCmd

		if len(args) > 0 {
			c = args
		}
	}

	return append(append([]string{}, e...), c...)
}

// MergeEnv appends spec over image, the later KEY replacing the earlier in place.
func MergeEnv(image []string, spec map[string]string, keys []string) []string {
	out := append([]string{}, image...)
	idx := map[string]int{}

	for i, kv := range out {
		k, _, _ := strings.Cut(kv, "=")
		idx[k] = i
	}

	for _, k := range keys {
		kv := k + "=" + spec[k]
		if i, ok := idx[k]; ok {
			out[i] = kv
			continue
		}

		idx[k] = len(out)
		out = append(out, kv)
	}

	return out
}
