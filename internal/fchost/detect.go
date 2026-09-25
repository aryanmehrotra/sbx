// Package fchost decides where a Firecracker microVM can run from the machine sbx is on, and
// when that is not this machine, runs one that can.
//
// Firecracker needs Linux KVM. Most people typing `sbx create --provider firecracker` are not
// on a Linux host with /dev/kvm: they are on a Mac, on Windows, or on a cloud VM whose
// instance type does not expose nested virtualisation. The answer for each is different and
// none of them is "silently use a container instead" (DECISIONS: isolation fails closed):
//
//	linux + /dev/kvm            direct            the provider drives firecracker itself
//	macOS, Apple M3+, macOS 15+ helper-vm         a lima/colima VM with nested virt runs it
//	Windows 11 + WSL2           helper-vm         a WSL2 distro with nested virt runs it
//	kubernetes                  kata-runtimeclass runtimeClassName on a kata-fc RuntimeClass
//	anything else               refused           with the reason and the fix
//
// The helper VM is ROADMAP §1 option B, and the spike behind it measured it on this Mac
// (docs/superpowers/specs/2026-09-26-firecracker-spike.md): a real /dev/kvm through colima
// --nested-virtualization on an M4, Firecracker unmodified, 88 ms restore to first byte.
//
// Detection reads what the host's own tools print, through Probe, so every branch - including
// Windows, where no host is available to test on - is exercised by a unit test on any GOOS.
package fchost

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Kind is which backend a microVM would use here. The names match the hostcap package the
// Linux-direct provider carries, so the two reconcile by aliasing rather than translating.
type Kind string

const (
	Direct           Kind = "direct"
	HelperVM         Kind = "helper-vm"
	KataRuntimeClass Kind = "kata-runtimeclass"
	Refused          Kind = "refused"
)

// Backend is the decision plus what a person needs to read about it.
type Backend struct {
	Kind Kind `json:"kind"`

	// Reason is why this backend: what was found, in the host's own words where possible.
	Reason string `json:"reason"`

	// Next is what to do about a refusal. Every refusal has one; a refusal without a next
	// step is just a wall.
	Next string `json:"next,omitempty"`

	// Helper names the tool that runs the helper VM: lima, colima or wsl.
	Helper string `json:"helper,omitempty"`
}

// Probe is the host, as far as detection is concerned. Only read-only questions: nothing here
// may create, start or change anything, because `sbx doctor` calls it.
type Probe struct {
	GOOS, GOARCH string

	// Output runs a read-only command and returns its stdout.
	Output func(name string, args ...string) (string, error)

	ReadFile   func(path string) ([]byte, error)
	CharDevice func(path string) bool
	LookPath   func(name string) bool
	Getenv     func(key string) string

	// Home is the user's home directory: %USERPROFILE% on Windows, where .wslconfig lives.
	Home string
}

// Host is the real machine.
func Host() Probe {
	home, _ := os.UserHomeDir()

	return Probe{
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Output: func(name string, args ...string) (string, error) {
			out, err := exec.Command(name, args...).Output()

			return string(out), err
		},
		ReadFile: os.ReadFile,
		CharDevice: func(p string) bool {
			fi, err := os.Stat(p)

			return err == nil && fi.Mode()&os.ModeCharDevice != 0
		},
		LookPath: func(n string) bool {
			_, err := exec.LookPath(n)

			return err == nil
		},
		Getenv: os.Getenv,
		Home:   home,
	}
}

// RuntimeClass is the RuntimeClass `--isolation firecracker` asks a cluster for. The provider
// owns the name, because it is the one that puts it in the pod spec.
func RuntimeClass(getenv func(string) string) string {
	return provider.FirecrackerRuntimeClass(getenv)
}

// ForProvider is Detect for a provider kind: kubernetes never runs a VMM on this machine, so
// what this machine has does not matter to it.
func ForProvider(kind string, p Probe) Backend {
	switch kind {
	case "kubernetes", "k8s":
		rc := RuntimeClass(p.Getenv)

		return Backend{
			Kind: KataRuntimeClass,
			Reason: fmt.Sprintf("pods get runtimeClassName %s; the cluster must have that RuntimeClass "+
				"(checked before anything is created)", rc),
		}
	default:
		return Detect(p)
	}
}

// Detect decides for this machine.
func Detect(p Probe) Backend {
	switch p.GOOS {
	case "linux":
		return detectLinux(p)
	case "darwin":
		return detectDarwin(p)
	case "windows":
		return detectWindows(p)
	default:
		return Backend{
			Kind:   Refused,
			Reason: fmt.Sprintf("Firecracker needs Linux KVM, and %s has none (bhyve is not a backend sbx drives)", p.GOOS),
			Next:   "run sbx on a Linux host with /dev/kvm, or use --provider docker here",
		}
	}
}

// --- linux ------------------------------------------------------------------------------------

func detectLinux(p Probe) Backend {
	if p.CharDevice("/dev/kvm") {
		// Presence only. Whether this process can open it and KVM_GET_API_VERSION answers 12
		// is the direct provider's probe to make - it is the one about to use it.
		return Backend{Kind: Direct, Reason: "/dev/kvm is present"}
	}

	generic := "enable nested virtualisation on this instance, move to a bare-metal instance type, " +
		"or run with --isolation gvisor|kata instead of a microVM"

	// Inside WSL2 the fix is one line on the Windows side, and it is a different line from
	// every cloud's.
	if v, err := p.ReadFile("/proc/version"); err == nil && strings.Contains(strings.ToLower(string(v)), "microsoft") {
		return Backend{
			Kind:   Refused,
			Reason: "no /dev/kvm in this WSL2 distro: nested virtualisation is off",
			Next:   wslNestedFix + ", or " + generic[strings.Index(generic, "run with"):],
		}
	}

	vendor := firstLine(p.ReadFile, "/sys/class/dmi/id/sys_vendor")
	product := firstLine(p.ReadFile, "/sys/class/dmi/id/product_name")

	where := "this host"
	if vendor != "" {
		where = strings.TrimSpace(vendor + " " + product)
	}

	reason := fmt.Sprintf("no /dev/kvm on %s, so Firecracker cannot run here", where)

	// The CPU can do it and the kernel was never told: the cheapest fix of all.
	if info, err := p.ReadFile("/proc/cpuinfo"); err == nil && cpuHasVirt(string(info)) {
		return Backend{
			Kind:   Refused,
			Reason: reason + " - the CPU advertises vmx/svm, so the kvm module is not loaded",
			Next:   "sudo modprobe kvm_intel (or kvm_amd), or " + generic,
		}
	}

	next := generic

	switch {
	case strings.Contains(vendor, "Amazon"):
		next = "EC2 exposes KVM only on .metal instance types (or instance types launched with nested " +
			"virtualisation enabled); move to one, or run with --isolation gvisor|kata"
	case strings.Contains(vendor, "Google"):
		next = "recreate the instance with --enable-nested-virtualization (an Intel N1/N2/C2/C3 machine type), " +
			"or run with --isolation gvisor|kata"
	case strings.Contains(vendor, "Microsoft"):
		next = "on Azure pick a size that supports nested virtualisation (Dv3/Ev3 and later), or run with " +
			"--isolation gvisor|kata"
	}

	return Backend{Kind: Refused, Reason: reason, Next: next}
}

func cpuHasVirt(cpuinfo string) bool {
	sc := bufio.NewScanner(strings.NewReader(cpuinfo))
	for sc.Scan() {
		name, val, ok := strings.Cut(sc.Text(), ":")
		if !ok || strings.TrimSpace(name) != "flags" {
			continue
		}

		for _, f := range strings.Fields(val) {
			if f == "vmx" || f == "svm" {
				return true
			}
		}
	}

	return false
}

func firstLine(read func(string) ([]byte, error), path string) string {
	b, err := read(path)
	if err != nil {
		return ""
	}

	line, _, _ := strings.Cut(string(b), "\n")

	return strings.TrimSpace(line)
}

// --- darwin -----------------------------------------------------------------------------------

// AssumeNestedEnv lets a chip whose name this code cannot parse through. It does not unlock a
// chip it can parse and knows is too old: the override is for the future, not for arguing.
const AssumeNestedEnv = "SBX_FC_ASSUME_NESTED"

// DriverEnv forces lima or colima when both are installed.
const DriverEnv = "SBX_FC_VM_DRIVER"

var chipRE = regexp.MustCompile(`\bApple M(\d+)\b`)

// chipGeneration is 3 for "Apple M3 Pro", and 0 when the string names no M-series chip.
func chipGeneration(brand string) int {
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

func detectDarwin(p Probe) Backend {
	if p.GOARCH != "arm64" {
		return Backend{
			Kind: Refused,
			Reason: "an Intel Mac: Virtualization.framework offers nested virtualisation only on Apple M3 " +
				"and later, so no Linux VM here can have /dev/kvm",
			Next: "run microVM sandboxes on a Linux host with /dev/kvm, or use --provider docker here",
		}
	}

	brand, _ := p.Output("sysctl", "-n", "machdep.cpu.brand_string")
	brand = strings.TrimSpace(brand)

	ver, _ := p.Output("sw_vers", "-productVersion")
	ver = strings.TrimSpace(ver)

	gen := chipGeneration(brand)

	switch {
	case gen == 0 && p.Getenv(AssumeNestedEnv) == "":
		return Backend{
			Kind:   Refused,
			Reason: fmt.Sprintf("could not tell the chip generation from %q, and nested virtualisation needs Apple M3 or later", brand),
			Next:   fmt.Sprintf("if this is an M3 or later, set %s=1", AssumeNestedEnv),
		}
	case gen > 0 && gen < 3:
		return Backend{
			Kind: Refused,
			Reason: fmt.Sprintf("%s: Virtualization.framework offers nested virtualisation only on Apple M3 and later, "+
				"so a Linux VM here cannot have /dev/kvm", brand),
			Next: "run microVM sandboxes on a Linux host with /dev/kvm (or an M3+ Mac), or use --provider docker here",
		}
	}

	if majorVersion(ver) < 15 {
		return Backend{
			Kind:   Refused,
			Reason: fmt.Sprintf("macOS %s: nested virtualisation needs macOS 15 or later", orUnknown(ver)),
			Next:   "update macOS to 15 or later, or use --provider docker here",
		}
	}

	helper, refusal := pickDarwinHelper(p)
	if refusal != nil {
		return *refusal
	}

	return Backend{
		Kind:   HelperVM,
		Helper: helper,
		Reason: fmt.Sprintf("%s on macOS %s: a %s VM with nested virtualisation provides /dev/kvm", brand, ver, helper),
	}
}

func pickDarwinHelper(p Probe) (string, *Backend) {
	bin := map[string]string{"lima": "limactl", "colima": "colima"}

	if want := strings.TrimSpace(p.Getenv(DriverEnv)); want != "" {
		b, known := bin[want]
		if !known {
			return "", &Backend{
				Kind:   Refused,
				Reason: fmt.Sprintf("%s=%s is not a VM driver sbx knows", DriverEnv, want),
				Next:   fmt.Sprintf("set %s to lima or colima, or unset it", DriverEnv),
			}
		}

		if !p.LookPath(b) {
			return "", &Backend{
				Kind:   Refused,
				Reason: fmt.Sprintf("%s=%s, and %s is not on PATH", DriverEnv, want, b),
				Next:   fmt.Sprintf("brew install %s, or unset %s", want, DriverEnv),
			}
		}

		return want, nil
	}

	// lima first: it takes the port-forward rules and the template on the command line, where
	// colima needs its own config. colima is lima underneath, so either gives the same VM.
	if p.LookPath("limactl") {
		return "lima", nil
	}

	if p.LookPath("colima") {
		return "colima", nil
	}

	return "", &Backend{
		Kind:   Refused,
		Reason: "this Mac can run a nested-virtualisation VM, but neither limactl nor colima is installed to run it",
		Next:   "brew install lima (or colima), then run the command again",
	}
}

// --- windows ----------------------------------------------------------------------------------

// wslNestedFix is the exact edit, pasteable. WSL2 turns nested virtualisation on by default on
// Windows 11, so a machine needing this line is one where somebody turned it off.
const wslNestedFix = "add `nestedVirtualization=true` under `[wsl2]` in %USERPROFILE%\\.wslconfig, then run `wsl --shutdown`"

var winBuildRE = regexp.MustCompile(`\[Version \d+\.\d+\.(\d+)`)

func detectWindows(p Probe) Backend {
	out, _ := p.Output("cmd", "/c", "ver")

	build := 0
	if m := winBuildRE.FindStringSubmatch(out); m != nil {
		build, _ = strconv.Atoi(m[1])
	}

	// 22000 is the first Windows 11 build; nested virtualisation in WSL2 is Windows 11 only.
	if build < 22000 {
		return Backend{
			Kind:   Refused,
			Reason: fmt.Sprintf("Windows build %d: nested virtualisation in WSL2 needs Windows 11 (build 22000+)", build),
			Next:   "upgrade to Windows 11, or use --provider docker",
		}
	}

	if !p.LookPath("wsl.exe") {
		return Backend{
			Kind:   Refused,
			Reason: "WSL is not installed, and the microVM runs inside a WSL2 distro",
			Next:   "run `wsl --install` in an elevated terminal, reboot, then run the command again",
		}
	}

	if b, err := p.ReadFile(p.Home + "/.wslconfig"); err == nil && wslNestedOff(string(b)) {
		return Backend{
			Kind:   Refused,
			Reason: ".wslconfig turns nested virtualisation off, so no WSL2 distro has /dev/kvm",
			Next:   wslNestedFix,
		}
	}

	return Backend{
		Kind:   HelperVM,
		Helper: "wsl",
		Reason: fmt.Sprintf("Windows build %d with WSL2: a WSL2 distro with nested virtualisation provides /dev/kvm", build),
	}
}

// wslNestedOff reads .wslconfig the way WSL does: an ini file whose key only counts under
// [wsl2], case-insensitive, whitespace around '=' allowed.
func wslNestedOff(ini string) bool {
	section := ""
	off := false

	sc := bufio.NewScanner(strings.NewReader(ini))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		switch {
		case line == "", strings.HasPrefix(line, "#"), strings.HasPrefix(line, ";"):
			continue
		case strings.HasPrefix(line, "["):
			section = strings.ToLower(strings.Trim(line, "[] "))
			continue
		}

		k, v, ok := strings.Cut(line, "=")
		if !ok || section != "wsl2" || !strings.EqualFold(strings.TrimSpace(k), "nestedVirtualization") {
			continue
		}

		off = strings.EqualFold(strings.TrimSpace(v), "false")
	}

	return off
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown version)"
	}

	return s
}
