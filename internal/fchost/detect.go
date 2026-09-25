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

	"github.com/aryanmehrotra/sbx/internal/fc/hostcap"
	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Kind is which backend a microVM would use here: hostcap's, not a copy of it. hostcap decides
// Linux and macOS for the provider, doctor and this package alike; the names cannot drift apart
// because there is only one set.
type Kind = hostcap.Backend

const (
	Direct           = hostcap.Direct
	HelperVM         = hostcap.HelperVM
	KataRuntimeClass = hostcap.KataRuntimeClass
	Refused          = hostcap.Refused
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

	// KVM opens /dev/kvm and asks its API version (hostcap.ProbeKVM). Nil in a fake means
	// CharDevice("/dev/kvm") stands in for it: present is taken as usable.
	KVM func() hostcap.KVM

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
		KVM: hostcap.ProbeKVM,
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

// Detect decides for this machine. Linux and macOS are hostcap's decision, made from a Report
// this package fills from its own fakeable Probe; what is left here is what hostcap does not
// own: which tool runs the helper VM on a Mac (lima or colima), the SBX_FC_ASSUME_NESTED
// override (hostcap never reads the environment), and the whole Windows/WSL branch.
func Detect(p Probe) Backend {
	if p.GOOS == "windows" {
		return detectWindows(p)
	}

	d := hostcap.Decide(report(p))
	b := Backend{Kind: d.Backend, Reason: d.Reason, Next: d.Next}

	if b.Kind == HelperVM && p.GOOS == "darwin" {
		helper, refusal := pickDarwinHelper(p)
		if refusal != nil {
			return *refusal
		}

		b.Helper = helper
		b.Reason += "; sbx runs that VM with " + helper
	}

	return b
}

// report is hostcap.Probe, answered through p so every host shape is testable anywhere.
func report(p Probe) hostcap.Report {
	r := hostcap.Report{OS: p.GOOS, Arch: p.GOARCH}

	switch p.GOOS {
	case "linux":
		if p.KVM != nil {
			r.KVM = p.KVM()
		} else if p.CharDevice("/dev/kvm") {
			r.KVM = hostcap.KVM{Present: true, Usable: true, APIVersion: hostcap.KVMAPIVersion}
		} else {
			r.KVM = hostcap.KVM{Detail: "/dev/kvm does not exist"}
		}

		r.Vendor = firstLine(p.ReadFile, "/sys/class/dmi/id/sys_vendor")
		r.Product = firstLine(p.ReadFile, "/sys/class/dmi/id/product_name")

		if v, err := p.ReadFile("/proc/version"); err == nil {
			r.WSL = strings.Contains(strings.ToLower(string(v)), "microsoft")
		}

		cpuinfo, _ := p.ReadFile("/proc/cpuinfo")
		r.CPUVirt = hostcap.CPUVirt(string(cpuinfo))
		r.Guest, r.GuestHint = hostcap.GuestEvidence(r.Vendor, r.Product, string(cpuinfo))
	case "darwin":
		brand, _ := p.Output("sysctl", "-n", "machdep.cpu.brand_string")
		ver, _ := p.Output("sw_vers", "-productVersion")
		r.CPUBrand, r.OSVersion = strings.TrimSpace(brand), strings.TrimSpace(ver)
		r.AssumeNested = p.Getenv(AssumeNestedEnv) != ""
	}

	return r
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

// AssumeNestedEnv lets a chip whose name hostcap cannot parse through. It does not unlock a chip
// it can parse and knows is too old: the override is for the future, not for arguing.
const AssumeNestedEnv = hostcap.AssumeNestedEnv

// DriverEnv forces lima or colima when both are installed.
const DriverEnv = "SBX_FC_VM_DRIVER"

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

// wslNestedFix is hostcap's, which the WSL2-inside-linux refusal quotes too.
const wslNestedFix = hostcap.WSLNestedFix

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
