package hostcap

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// kvmGetAPIVersion is _IO(KVMIO, 0x00) with KVMIO = 0xAE: the first ioctl anything that uses KVM
// makes, and the cheapest proof that the device node is a working KVM and not merely a file.
const kvmGetAPIVersion = 0xAE00

func kvmProbe() KVM {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return KVM{Detail: "/dev/kvm does not exist"}
		case errors.Is(err, os.ErrPermission):
			return KVM{Present: true, Detail: "permission denied opening /dev/kvm read-write"}
		default:
			return KVM{Present: true, Detail: err.Error()}
		}
	}
	defer f.Close()

	v, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), kvmGetAPIVersion, 0)
	if errno != 0 {
		return KVM{Present: true, Detail: "KVM_GET_API_VERSION failed: " + errno.Error()}
	}

	k := KVM{Present: true, APIVersion: int(v), Usable: int(v) == KVMAPIVersion}
	if !k.Usable {
		k.Detail = "unexpected KVM API version"
	}

	return k
}

// nestedProbe says whether THIS host's KVM will pass virtualisation on to its own guests. It is
// informational on Linux - Direct does not need it - and is what `sbx doctor` shows so that
// somebody planning to run the helper-VM path on a Linux box knows whether it could.
func nestedProbe() (bool, string) {
	for _, p := range []string{
		"/sys/module/kvm_intel/parameters/nested",
		"/sys/module/kvm_amd/parameters/nested",
	} {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}

		v := strings.TrimSpace(string(b))

		return v == "Y" || v == "1", p + "=" + v
	}

	return false, "no nested parameter exposed by the kvm module"
}

func guestProbe() (bool, string) {
	vendor, product, _, _ := linuxProbe()
	cpuinfo, _ := os.ReadFile("/proc/cpuinfo")

	return GuestEvidence(vendor, product, string(cpuinfo))
}

// linuxProbe reads the evidence a refusal is worded from: DMI (which cloud), /proc/version (a
// WSL2 kernel) and /proc/cpuinfo (vmx/svm present with no module loaded).
func linuxProbe() (vendor, product string, wsl, cpuVirt bool) {
	line := func(p string) string {
		b, _ := os.ReadFile(p)
		l, _, _ := strings.Cut(string(b), "\n")

		return strings.TrimSpace(l)
	}

	vendor, product = line("/sys/class/dmi/id/sys_vendor"), line("/sys/class/dmi/id/product_name")

	if v, err := os.ReadFile("/proc/version"); err == nil {
		wsl = strings.Contains(strings.ToLower(string(v)), "microsoft")
	}

	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		cpuVirt = CPUVirt(string(b))
	}

	return vendor, product, wsl, cpuVirt
}

func macProbe() (brand, version string) { return "", "" }
