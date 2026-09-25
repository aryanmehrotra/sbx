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
	"regexp"
	"runtime"
	"slices"
	"strconv"
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

	// A Mac's chip and macOS version, as the probe read them; Decide parses them. Empty off darwin.
	CPUBrand  string `json:"cpu_brand,omitempty"`
	OSVersion string `json:"os_version,omitempty"`

	// AssumeNested is set by the CALLER (fchost, from SBX_FC_ASSUME_NESTED) for a Mac whose chip
	// name Decide cannot parse. It never unlocks a chip Decide can parse and knows is too old:
	// the override is for the future, not for arguing.
	AssumeNested bool `json:"assume_nested,omitempty"`

	// Linux evidence that turns "no /dev/kvm" into the right next step: the DMI vendor and
	// product (which cloud), a WSL2 kernel (the fix is on the Windows side), and a CPU that
	// advertises vmx/svm (the module is simply not loaded).
	Vendor  string `json:"vendor,omitempty"`
	Product string `json:"product,omitempty"`
	WSL     bool   `json:"wsl,omitempty"`
	CPUVirt bool   `json:"cpu_virt,omitempty"`
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
	probeMac    = macProbe
	probeLinux  = linuxProbe
	lookPath    = exec.LookPath
)

// Probe reads the machine. It does not decide anything; see Decide.
// ProbeKVM is only the /dev/kvm question: open it read-write and ask KVM_GET_API_VERSION.
func ProbeKVM() KVM { return probeKVM() }

func Probe() Report {
	r := Report{OS: runtime.GOOS, Arch: runtime.GOARCH}
	r.KVM = probeKVM()
	r.Nested, r.NestedHint = probeNested()
	r.Guest, r.GuestHint = probeGuest()
	r.CPUBrand, r.OSVersion = probeMac()
	r.Vendor, r.Product, r.WSL, r.CPUVirt = probeLinux()

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
	case r.WSL:
		// Inside WSL2 the fix is one line on the Windows side, and a different line from
		// every cloud's.
		return Decision{Backend: Refused,
			Reason: "no /dev/kvm in this WSL2 distro: nested virtualisation is off",
			Next:   WSLNestedFix + ", or " + noMicroVM}
	case r.CPUVirt:
		// The CPU can do it and the kernel was never told: the cheapest fix of all.
		return Decision{Backend: Refused,
			Reason: "no /dev/kvm on " + r.where() + " - the CPU advertises vmx/svm, so the kvm module is not loaded",
			Next:   "sudo modprobe kvm_intel (or kvm_amd), or " + noMicroVM}
	case r.Guest || cloudFix(r.Vendor) != cloudFix(""):
		// Inside a VM whose hypervisor did not pass virtualisation through. Worth saying
		// precisely, because the fix is on the OTHER machine - and each cloud spells it
		// differently.
		return Decision{Backend: Refused,
			Reason: "no /dev/kvm: this Linux is itself a virtual machine (" + r.guestName() +
				") and its hypervisor does not expose virtualisation to it",
			Next: cloudFix(r.Vendor)}
	default:
		return Decision{Backend: Refused,
			Reason: "no /dev/kvm on " + r.where() + " (" + r.KVM.Detail + "), so Firecracker cannot run here",
			Next: "load the module (`sudo modprobe kvm_intel` or `kvm_amd`; on arm64 it is built in) " +
				"and enable virtualisation in the firmware; on a VM, enable nested virtualisation or " +
				"move to a bare-metal instance type; or " + noMicroVM}
	}
}

// noMicroVM is the way out that needs no KVM at all.
const noMicroVM = "run with --isolation gvisor|kata instead of a microVM, or use --provider docker"

// WSLNestedFix is the exact edit, pasteable. WSL2 turns nested virtualisation on by default on
// Windows 11, so a machine needing this line is one where somebody turned it off.
const WSLNestedFix = "add `nestedVirtualization=true` under `[wsl2]` in %USERPROFILE%\\.wslconfig, then run `wsl --shutdown`"

func (r Report) guestName() string {
	switch w := r.where(); {
	case r.GuestHint == "":
		return w
	case w == "this host" || strings.Contains(w, r.GuestHint):
		return r.GuestHint
	default:
		return r.GuestHint + ", " + w
	}
}

func (r Report) where() string {
	if w := strings.TrimSpace(r.Vendor + " " + r.Product); w != "" {
		return w
	}

	return "this host"
}

// cloudFix is how each cloud that sbx has been asked about exposes KVM to a VM.
func cloudFix(vendor string) string {
	switch {
	case strings.Contains(vendor, "Amazon"):
		return "EC2 exposes KVM only on .metal instance types (or instance types launched with nested " +
			"virtualisation enabled); move to one, or " + noMicroVM
	case strings.Contains(vendor, "Google"):
		return "recreate the instance with --enable-nested-virtualization (an Intel N1/N2/C2/C3 machine " +
			"type), or " + noMicroVM
	case strings.Contains(vendor, "Microsoft"):
		return "on Azure pick a size that supports nested virtualisation (Dv3/Ev3 and later), or " + noMicroVM
	default:
		return "enable nested virtualisation for this VM on its host (colima: `--nested-virtualization`; " +
			"a cloud VM: an instance type or flag that allows it), move to a bare-metal instance type, or " +
			noMicroVM
	}
}

// AssumeNestedEnv is how a person lets through a Mac chip this code cannot name. hostcap never
// reads the environment; the caller does and sets Report.AssumeNested.
const AssumeNestedEnv = "SBX_FC_ASSUME_NESTED"

// decideDarwin: nested virtualisation came to Virtualization.framework in macOS 15, on Apple M3
// and later. Each way of missing that is its own refusal, because each has its own fix.
func decideDarwin(r Report) Decision {
	if r.Arch != "arm64" {
		return Decision{Backend: Refused,
			Reason: "an Intel Mac: Virtualization.framework offers nested virtualisation only on Apple M3 " +
				"and later, so no Linux VM here can have /dev/kvm",
			Next: "run microVM sandboxes on a Linux host with /dev/kvm, or use --provider docker here"}
	}

	gen := ChipGeneration(r.CPUBrand)

	switch {
	case gen == 0 && !r.AssumeNested:
		return Decision{Backend: Refused,
			Reason: fmt.Sprintf("could not tell the chip generation from %q, and nested virtualisation "+
				"needs Apple M3 or later", r.CPUBrand),
			Next: fmt.Sprintf("if this is an M3 or later, set %s=1; otherwise use --provider docker here", AssumeNestedEnv)}
	case gen > 0 && gen < 3:
		return Decision{Backend: Refused,
			Reason: fmt.Sprintf("%s: Virtualization.framework offers nested virtualisation only on Apple "+
				"M3 and later, so a Linux VM here cannot have /dev/kvm", r.CPUBrand),
			Next: "use --provider docker here - a stopped container is the sleep state it has - or run " +
				"microVM sandboxes on a Linux host with /dev/kvm (or an M3+ Mac)"}
	case majorVersion(r.OSVersion) < 15:
		return Decision{Backend: Refused,
			Reason: fmt.Sprintf("macOS %s: nested virtualisation needs macOS 15 or later", orUnknown(r.OSVersion)),
			Next:   "update macOS to 15 or later, or use --provider docker here"}
	}

	return Decision{Backend: HelperVM, Reason: fmt.Sprintf("%s on macOS %s: macOS has no KVM, but a "+
		"Linux VM with nested virtualisation here has a working /dev/kvm", r.CPUBrand, r.OSVersion)}
}

var chipRE = regexp.MustCompile(`\bApple M(\d+)\b`)

// ChipGeneration is 3 for "Apple M3 Pro", and 0 when the string names no M-series chip.
func ChipGeneration(brand string) int {
	m := chipRE.FindStringSubmatch(brand)
	if m == nil {
		return 0
	}

	n, _ := strconv.Atoi(m[1])

	return n
}

func majorVersion(v string) int {
	head, _, _ := strings.Cut(strings.TrimSpace(v), ".")
	n, _ := strconv.Atoi(head)

	return n
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown version)"
	}

	return s
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

	return ChipGeneration(brand) >= 3 && majorVersion(version) >= 15, hint
}

// GuestEvidence says whether a Linux with these DMI strings and this /proc/cpuinfo is itself a
// virtual machine, and names the hypervisor when it can. Exported so a caller that reads the
// machine through its own fakeable probe (fchost) reaches the same verdict as Probe.
func GuestEvidence(vendor, product, cpuinfo string) (bool, string) {
	if name := knownHypervisor(vendor, product); name != "" {
		return true, name
	}

	if hypervisorFlag(cpuinfo) {
		return true, "cpuinfo carries the hypervisor flag"
	}

	return false, ""
}

// CPUVirt reports whether /proc/cpuinfo advertises vmx or svm: hardware virtualisation the
// kernel could use if the kvm module were loaded.
func CPUVirt(cpuinfo string) bool { return cpuFlag(cpuinfo, "vmx") || cpuFlag(cpuinfo, "svm") }

// hypervisorFlag reports whether /proc/cpuinfo carries the x86 "hypervisor" flag, which the CPU
// sets for any guest. arm64 has no such flag; DMI is the evidence there.
func hypervisorFlag(cpuinfo string) bool { return cpuFlag(cpuinfo, "hypervisor") }

func cpuFlag(cpuinfo, flag string) bool {
	for _, line := range strings.Split(cpuinfo, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(k) != "flags" {
			continue
		}

		if slices.Contains(strings.Fields(v), flag) {
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
