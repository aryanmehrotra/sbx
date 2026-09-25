package hostcap

import "syscall"

func kvmProbe() KVM { return KVM{Detail: "macOS has no KVM"} }

func macProbe() (brand, version string) {
	brand, _ = syscall.Sysctl("machdep.cpu.brand_string")
	version, _ = syscall.Sysctl("kern.osproductversion")

	return brand, version
}

func nestedProbe() (bool, string) { return appleNests(macProbe()) }

func guestProbe() (bool, string) { return false, "" }

func linuxProbe() (string, string, bool, bool) { return "", "", false, false }
