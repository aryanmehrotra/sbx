// Package hostcap answers one question before anything microVM-shaped is attempted: on this
// machine, which way - if any - can a Firecracker VM be run?
//
// It is one small package, not a check scattered through the provider, because three callers
// ask the same question and must get the same answer: `sbx doctor` reports it, `--provider
// firecracker` acts on it, and the helper-VM layer (sbx serve inside a nested-virtualisation
// Linux VM on macOS and Windows) runs the very same probe inside its guest, where the answer is
// expected to be Direct. A second copy of the logic would disagree with the first the day one of
// them learned about a new chip.
//
// It never changes the host. Every probe reads; the answer to "no /dev/kvm" is a sentence
// naming what to do, not a modprobe.
package hostcap

import (
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"
)

// Backend is how a Firecracker VM gets run here.
type Backend string

const (
	// Direct: this is Linux with a usable /dev/kvm, so firecracker runs as a child of sbx.
	Direct Backend = "direct"

	// HelperVM: this host cannot run Firecracker itself but can run a Linux VM that can, through
	// nested virtualisation. A separate layer owns that VM and decides whether it works; this
	// package only says it is the path worth trying.
	HelperVM Backend = "helper-vm"

	// KataRuntimeClass: no KVM here, but the cluster the caller is pointed at already offers a
	// kata RuntimeClass, so a VM boundary is available without sbx running a VMM at all.
	KataRuntimeClass Backend = "kata-runtimeclass"

	// Refused: no path. Decision.Reason says why and Decision.Next what would change it.
	Refused Backend = "refused"
)

// KVMAPIVersion is the only value KVM_GET_API_VERSION has returned since Linux 2.6.22, and the
// one Firecracker checks for. Anything else is a kernel that is not what it claims.
const KVMAPIVersion = 12

// KVM is what /dev/kvm said when it was asked.
type KVM struct {
	Present    bool   `json:"present"`     // the device node exists
	Usable     bool   `json:"usable"`      // this process could open it read-write and the ioctl answered 12
	APIVersion int    `json:"api_version"` // KVM_GET_API_VERSION, 0 when it could not be asked
	Detail     string `json:"detail"`      // the errno or oddity, in words
}

// Report is everything the probes found, before any decision is taken on it.
type Report struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	KVM  KVM    `json:"kvm"`

	// Nested is whether this host can plausibly give a Linux VM a working /dev/kvm, and
	// NestedHint is the evidence in words ("Apple M4, macOS 26.4"). A hint, not a proof: the
	// only proof is booting the helper VM and running this same probe inside it.
	Nested     bool   `json:"nested"`
	NestedHint string `json:"nested_hint"`

	// Guest is true when this Linux is itself a virtual machine, and GuestHint names the
	// hypervisor when it can be read. Only consulted when there is no /dev/kvm, where it turns
	// "load the module" into the right advice: enable nesting on the machine underneath.
	Guest     bool   `json:"guest"`
	GuestHint string `json:"guest_hint,omitempty"`

	// Mkfs is the path to mkfs.ext4 (e2fsprogs), or "" when absent. The rootfs pipeline needs
	// it on whichever machine runs firecracker, so inside the helper VM it is that VM's copy.
	Mkfs string `json:"mkfs"`

	// KataRuntimeClass is set by the CALLER when it knows a cluster offers one. hostcap never
	// asks a cluster: that is a network call to somebody else's API, and this package is meant
	// to answer in microseconds on a machine with no network at all.
	KataRuntimeClass bool `json:"kata_runtimeclass"`
}

// Decision is what to do with a Report.
type Decision struct {
	Backend Backend `json:"backend"`
	Reason  string  `json:"reason"`         // why this backend, or why none
	Next    string  `json:"next,omitempty"` // for Refused: the one thing that would change the answer
}

// Err is nil unless the decision is a refusal, in which case it is the refusal as an error
// that says what to do next.
func (d Decision) Err() error {
	if d.Backend != Refused {
		return nil
	}

	if d.Next == "" {
		return fmt.Errorf("firecracker cannot run here: %s", d.Reason)
	}

	return fmt.Errorf("firecracker cannot run here: %s - %s", d.Reason, d.Next)
}

// The probes, as variables so a test can say what the machine looks like without owning one.
var (
	probeKVM    = kvmProbe
	probeNested = nestedProbe
	probeGuest  = guestProbe
	lookPath    = exec.LookPath
)

// Probe reads the machine. It does not decide anything; see Decide.
func Probe() Report {
	r := Report{OS: runtime.GOOS, Arch: runtime.GOARCH}
	r.KVM = probeKVM()
	r.Nested, r.NestedHint = probeNested()
	r.Guest, r.GuestHint = probeGuest()

	if p, err := lookPath("mkfs.ext4"); err == nil {
		r.Mkfs = p
	}

	return r
}

// Decide turns a report into a backend. Pure, so every branch is tested from a table.
func Decide(r Report) Decision {
	switch r.OS {
	case "linux":
		return decideLinux(r)
	case "darwin":
		return decideDarwin(r)
	case "windows":
		// Nothing on this side can prove Hyper-V will nest - that is a property of the VM
		// configuration the helper layer creates, not of anything readable from here - so the
		// path is offered and the helper layer is the one that finds out.
		return Decision{Backend: HelperVM, Reason: "Windows has no KVM; a Linux helper VM with " +
			"nested virtualisation can run Firecracker. " + r.NestedHint}
	default:
		return Decision{Backend: Refused,
			Reason: fmt.Sprintf("Firecracker needs Linux KVM, and %s/%s has neither KVM nor a "+
				"helper-VM path in sbx", r.OS, r.Arch),
			Next: "use --provider docker, or run sbx on a Linux host with /dev/kvm"}
	}
}

func decideLinux(r Report) Decision {
	// Firecracker ships for these two and nothing else; a 386 kernel with KVM is still no host.
	if r.Arch != "amd64" && r.Arch != "arm64" {
		return Decision{Backend: Refused,
			Reason: "Firecracker is built for x86_64 and aarch64 only, and this is linux/" + r.Arch,
			Next:   "use --provider docker on this machine"}
	}

	if r.KVM.Usable && r.KVM.APIVersion == KVMAPIVersion {
		return Decision{Backend: Direct, Reason: fmt.Sprintf("/dev/kvm is usable (API version %d)", r.KVM.APIVersion)}
	}

	if r.KataRuntimeClass {
		return Decision{Backend: KataRuntimeClass, Reason: "no usable /dev/kvm here, but the " +
			"cluster offers a kata RuntimeClass, which is a VM boundary without a local VMM"}
	}

	switch {
	case r.KVM.Present && r.KVM.APIVersion != 0 && r.KVM.APIVersion != KVMAPIVersion:
		return Decision{Backend: Refused,
			Reason: fmt.Sprintf("/dev/kvm answered API version %d, not %d", r.KVM.APIVersion, KVMAPIVersion),
			Next:   "this kernel's KVM is not one Firecracker supports; use --provider docker"}
	case r.KVM.Present:
		return Decision{Backend: Refused,
			Reason: "/dev/kvm exists but this process cannot use it (" + r.KVM.Detail + ")",
			Next: "add yourself to its group - `sudo usermod -aG kvm $USER`, then log in again - " +
				"or run sbx as a user that can open /dev/kvm read-write"}
	case r.Guest:
		// Inside a VM whose hypervisor did not pass virtualisation through. Worth saying
		// precisely, because the fix is on the OTHER machine.
		return Decision{Backend: Refused,
			Reason: "no /dev/kvm: this Linux is itself a virtual machine (" + r.GuestHint +
				") and its hypervisor does not expose virtualisation to it",
			Next: "enable nested virtualisation for this VM on its host (colima: " +
				"`--nested-virtualization`; a cloud VM: an instance type or flag that allows it), " +
				"or use --provider docker"}
	default:
		return Decision{Backend: Refused,
			Reason: "no /dev/kvm (" + r.KVM.Detail + ")",
			Next: "load the module (`sudo modprobe kvm_intel` or `kvm_amd`; on arm64 it is " +
				"built in) and enable virtualisation in the firmware, or use --provider docker"}
	}
}

func decideDarwin(r Report) Decision {
	if r.Nested {
		return Decision{Backend: HelperVM, Reason: "macOS has no KVM, but this Mac can give a " +
			"Linux VM a working /dev/kvm through nested virtualisation (" + r.NestedHint + ")"}
	}

	return Decision{Backend: Refused,
		Reason: "macOS has no KVM, and this Mac cannot nest virtualisation for a Linux VM (" +
			r.NestedHint + "); that needs Apple M3 or later on macOS 15 or later",
		Next: "use --provider docker here - a stopped container is the sleep state it has - " +
			"or run sbx on a Linux host with /dev/kvm"}
}

// appleNests reads a Mac's CPU brand string and macOS version and says whether a Linux VM on it
// can have /dev/kvm. Apple added nested virtualisation to Virtualization.framework in macOS 15,
// on M3 and later; an M1 or M2 on any macOS, or an Intel Mac, cannot.
//
// Parsed from strings rather than asked of the framework, because asking means cgo and the
// Virtualization.framework entitlement, and this runs in `sbx doctor` on a plain static binary.
// An unrecognised brand is answered "no" with the brand quoted, so a future chip shows up as a
// wrong refusal somebody can report rather than as a guess that happened to be yes.
func appleNests(brand, version string) (bool, string) {
	hint := brand + ", macOS " + version

	var major int
	if _, err := fmt.Sscanf(version, "%d", &major); err != nil {
		return false, hint + " (macOS version unreadable)"
	}

	var gen int
	if _, err := fmt.Sscanf(brand, "Apple M%d", &gen); err != nil {
		return false, hint
	}

	return gen >= 3 && major >= 15, hint
}

// hypervisorFlag reports whether /proc/cpuinfo carries the x86 "hypervisor" flag, which the CPU
// sets for any guest. arm64 has no such flag; DMI is the evidence there.
func hypervisorFlag(cpuinfo string) bool {
	for _, line := range strings.Split(cpuinfo, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "flags" {
			continue
		}

		if slices.Contains(strings.Fields(v), "hypervisor") {
			return true
		}
	}

	return false
}

// knownHypervisor names the virtual platform from DMI's vendor and product strings, or "".
func knownHypervisor(vendor, product string) string {
	s := strings.ToLower(vendor + " " + product)

	for _, k := range []struct{ needle, name string }{
		{"qemu", "QEMU/KVM"}, {"kvm", "KVM"}, {"apple virtualization", "Apple Virtualization.framework"},
		{"vmware", "VMware"}, {"virtualbox", "VirtualBox"}, {"innotek", "VirtualBox"},
		{"microsoft corporation virtual", "Hyper-V"}, {"amazon ec2", "Amazon EC2"},
		{"google compute engine", "Google Compute Engine"}, {"xen", "Xen"}, {"parallels", "Parallels"},
	} {
		if strings.Contains(s, k.needle) {
			return k.name
		}
	}

	return ""
}
