//go:build !linux && !darwin

package hostcap

import "runtime"

func kvmProbe() KVM { return KVM{Detail: runtime.GOOS + " has no KVM"} }

// nestedProbe cannot tell from here whether Hyper-V (or anything else) will nest for a VM that
// does not exist yet; the helper-VM layer finds out by booting one. So it says so.
func nestedProbe() (bool, string) {
	return false, "unverified from " + runtime.GOOS + ": whether a Linux VM here gets /dev/kvm " +
		"depends on the hypervisor's nested-virtualisation setting"
}

func guestProbe() (bool, string) { return false, "" }

func macProbe() (brand, version string) { return "", "" }

func linuxProbe() (string, string, bool, bool) { return "", "", false, false }
