package hostcap

import "syscall"

func kvmProbe() KVM { return KVM{Detail: "macOS has no KVM"} }

func nestedProbe() (bool, string) {
	brand, _ := syscall.Sysctl("machdep.cpu.brand_string")
	version, _ := syscall.Sysctl("kern.osproductversion")

	return appleNests(brand, version)
}

func guestProbe() (bool, string) { return false, "" }
