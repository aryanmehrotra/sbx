package hostcap

import (
	"errors"
	"strings"
	"testing"
)

func TestDecide(t *testing.T) {
	usable := KVM{Present: true, Usable: true, APIVersion: 12}

	for _, tc := range []struct {
		name string
		r    Report
		want Backend
		says string // a fragment the reason or next step must carry
	}{
		{"linux arm64 with kvm", Report{OS: "linux", Arch: "arm64", KVM: usable}, Direct, "usable"},
		{"linux amd64 with kvm", Report{OS: "linux", Arch: "amd64", KVM: usable}, Direct, "API version 12"},
		{"linux 386 even with kvm", Report{OS: "linux", Arch: "386", KVM: usable}, Refused, "x86_64 and aarch64"},
		{"linux no kvm, kata offered", Report{OS: "linux", Arch: "amd64", KataRuntimeClass: true}, KataRuntimeClass, "kata"},
		{"linux kvm wrong api", Report{OS: "linux", Arch: "amd64", KVM: KVM{Present: true, APIVersion: 11}}, Refused, "API version 11"},
		{"linux kvm permission", Report{OS: "linux", Arch: "amd64", KVM: KVM{Present: true, Detail: "permission denied"}}, Refused, "usermod -aG kvm"},
		{"linux guest without nesting", Report{OS: "linux", Arch: "arm64", Guest: true, GuestHint: "QEMU/KVM"}, Refused, "nested virtualisation for this VM"},
		{"linux bare, module absent", Report{OS: "linux", Arch: "amd64", KVM: KVM{Detail: "/dev/kvm does not exist"}}, Refused, "modprobe"},
		{"mac M4 macOS 26", mac("Apple M4", "26.4.1"), HelperVM, "nested"},
		{"mac M3 Pro macOS 15.0", mac("Apple M3 Pro", "15.0"), HelperVM, "macOS 15.0"},
		{"mac M1", mac("Apple M1", "15.0"), Refused, "M3 and later"},
		{"mac M2 Max", mac("Apple M2 Max", "26.0"), Refused, "M3 and later"},
		{"mac M3 on macOS 14", mac("Apple M3", "14.7.1"), Refused, "macOS 15"},
		{"mac chip it cannot read", mac("Apple Silicon Z", "26.0"), Refused, AssumeNestedEnv},
		{"mac chip it cannot read, assumed", func() Report { r := mac("Apple Silicon Z", "26.0"); r.AssumeNested = true; return r }(), HelperVM, "Apple Silicon Z"},
		{"mac M1 assumed is still refused", func() Report { r := mac("Apple M1", "26.0"); r.AssumeNested = true; return r }(), Refused, "M3 and later"},
		{"mac unreadable chip, assumed, macOS 14", func() Report { r := mac("Apple Silicon Z", "14.0"); r.AssumeNested = true; return r }(), Refused, "macOS 15"},
		{"intel mac", Report{OS: "darwin", Arch: "amd64", CPUBrand: "Intel(R) Core(TM) i9"}, Refused, "Intel"},
		{"linux inside WSL2", Report{OS: "linux", Arch: "amd64", WSL: true}, Refused, "nestedVirtualization=true"},
		{"linux vmx but no module", Report{OS: "linux", Arch: "amd64", CPUVirt: true}, Refused, "modprobe kvm"},
		{"linux on EC2", Report{OS: "linux", Arch: "amd64", Guest: true, GuestHint: "Amazon EC2", Vendor: "Amazon EC2"}, Refused, ".metal"},
		{"linux on GCE", Report{OS: "linux", Arch: "amd64", Guest: true, GuestHint: "Google Compute Engine", Vendor: "Google"}, Refused, "--enable-nested-virtualization"},
		{"linux on Azure", Report{OS: "linux", Arch: "amd64", Guest: true, GuestHint: "Hyper-V", Vendor: "Microsoft Corporation"}, Refused, "Azure"},
		{"linux bare, no kvm", Report{OS: "linux", Arch: "amd64"}, Refused, "--isolation gvisor|kata"},
		{"windows", Report{OS: "windows", Arch: "amd64"}, HelperVM, "helper VM"},
		{"freebsd", Report{OS: "freebsd", Arch: "amd64"}, Refused, "freebsd/amd64"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.r)
			if d.Backend != tc.want {
				t.Fatalf("backend = %s, want %s (%+v)", d.Backend, tc.want, d)
			}

			if !strings.Contains(d.Reason+" "+d.Next, tc.says) {
				t.Fatalf("decision does not say %q: %+v", tc.says, d)
			}

			// A refusal must say what to do; nothing else may pretend to be an error.
			if (d.Err() != nil) != (tc.want == Refused) {
				t.Fatalf("Err() = %v for backend %s", d.Err(), d.Backend)
			}

			if tc.want == Refused && d.Next == "" {
				t.Fatalf("a refusal with no next step: %+v", d)
			}
		})
	}
}

func TestAppleNests(t *testing.T) {
	for _, tc := range []struct {
		brand, version string
		want           bool
	}{
		{"Apple M4", "26.4.1", true},
		{"Apple M3 Pro", "15.0", true},
		{"Apple M3", "14.6", false}, // the chip can, the OS cannot yet
		{"Apple M2 Max", "26.0", false},
		{"Apple M1", "15.2", false},
		{"Intel(R) Core(TM) i9-9980HK", "15.1", false},
		{"Apple M5", "", false}, // unreadable version is a no, never a guess
	} {
		got, hint := appleNests(tc.brand, tc.version)
		if got != tc.want {
			t.Errorf("appleNests(%q, %q) = %v, want %v", tc.brand, tc.version, got, tc.want)
		}

		if !strings.Contains(hint, tc.brand) {
			t.Errorf("hint %q does not name the chip", hint)
		}
	}
}

func TestGuestEvidence(t *testing.T) {
	if !hypervisorFlag("processor : 0\nflags\t\t: fpu vme hypervisor lahf_lm\n") {
		t.Error("hypervisor flag not found")
	}

	if hypervisorFlag("flags : fpu vme\nmodel name : hypervisor-lookalike\n") {
		t.Error("matched outside the flags line")
	}

	if got := knownHypervisor("QEMU", "Standard PC (Q35 + ICH9, 2009)"); got != "QEMU/KVM" {
		t.Errorf("qemu = %q", got)
	}

	if got := knownHypervisor("Apple Inc.", "Apple Virtualization Generic Platform"); got != "Apple Virtualization.framework" {
		t.Errorf("vz = %q", got)
	}

	if got := knownHypervisor("Dell Inc.", "PowerEdge R640"); got != "" {
		t.Errorf("bare metal named %q", got)
	}
}

func TestProbeUsesEveryProbe(t *testing.T) {
	saved := [...]any{probeKVM, probeNested, probeGuest, lookPath, probeMac, probeLinux}
	t.Cleanup(func() {
		probeMac = saved[4].(func() (string, string))
		probeLinux = saved[5].(func() (string, string, bool, bool))
		probeKVM = saved[0].(func() KVM)
		probeNested = saved[1].(func() (bool, string))
		probeGuest = saved[2].(func() (bool, string))
		lookPath = saved[3].(func(string) (string, error))
	})

	probeKVM = func() KVM { return KVM{Present: true, Usable: true, APIVersion: 12} }
	probeNested = func() (bool, string) { return true, "nests" }
	probeGuest = func() (bool, string) { return true, "QEMU/KVM" }
	lookPath = func(string) (string, error) { return "/sbin/mkfs.ext4", nil }
	probeMac = func() (string, string) { return "Apple M4", "26.4.1" }
	probeLinux = func() (string, string, bool, bool) { return "Amazon EC2", "m7i.large", true, true }

	r := Probe()
	if !r.KVM.Usable || !r.Nested || r.NestedHint != "nests" || !r.Guest || r.Mkfs != "/sbin/mkfs.ext4" ||
		r.CPUBrand != "Apple M4" || r.OSVersion != "26.4.1" || r.Vendor != "Amazon EC2" || r.Product != "m7i.large" || !r.WSL || !r.CPUVirt {
		t.Fatalf("report = %+v", r)
	}

	lookPath = func(string) (string, error) { return "", errors.New("absent") }
	if Probe().Mkfs != "" {
		t.Fatal("mkfs reported present when lookPath failed")
	}
}

func mac(brand, version string) Report {
	nested, hint := appleNests(brand, version)

	return Report{OS: "darwin", Arch: "arm64", CPUBrand: brand, OSVersion: version, Nested: nested, NestedHint: hint}
}

func TestChipGeneration(t *testing.T) {
	for in, want := range map[string]int{
		"Apple M1": 1, "Apple M3 Pro": 3, "Apple M4 Max": 4, "Apple M10 Ultra": 10,
		"Apple Silicon": 0, "": 0, "Intel(R) Core(TM) i9": 0,
	} {
		if got := ChipGeneration(in); got != want {
			t.Errorf("ChipGeneration(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestCPUVirt(t *testing.T) {
	if !CPUVirt("flags : fpu vmx sse\n") || !CPUVirt("flags\t: svm\n") || CPUVirt("flags : fpu sse\nmodel name : vmx-ish\n") {
		t.Fatal("CPUVirt misread cpuinfo")
	}
}
