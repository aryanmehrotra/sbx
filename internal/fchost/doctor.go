package fchost

import (
	"context"
	"fmt"
	"os"
)

// DoctorRow is the microVM line of `sbx doctor`: whether a microVM can run from here, which
// backend it would use and why, or what is missing. status asks the VM tool about the helper
// VM, read-only; it is only called for a helper-VM host.
func DoctorRow(ctx context.Context, b Backend, name string, status func(context.Context) (State, error)) (have bool, detail, meaning string) {
	switch b.Kind {
	case Direct:
		return true, "direct: " + b.Reason, ""
	case KataRuntimeClass:
		return true, "kata-runtimeclass: " + b.Reason, ""
	case HelperVM:
		st, err := status(ctx)
		if err != nil {
			return false, fmt.Sprintf("helper VM (%s): %s; but asking about %s failed: %v", b.Helper, b.Reason, name, err),
				"--provider firecracker cannot start its VM until that works; `sbx fc vm status` repeats the question"
		}

		var vm string

		switch st {
		case Absent:
			vm = name + " not created yet - made on first use"
		case Stopped:
			vm = name + " stopped - started on demand"
		default:
			vm = name + " running"
		}

		return true, fmt.Sprintf("helper VM (%s): %s; %s", b.Helper, b.Reason, vm), ""
	default:
		return false, "refused: " + b.Reason, "--provider firecracker is refused here; " + b.Next
	}
}

// HostDoctorRow is DoctorRow for b, the decision HostBackend made for this machine; the caller
// passes it so the rows under it (mkfs.ext4, ip_forward) describe the same decision.
func HostDoctorRow(ctx context.Context, b Backend) (have bool, detail, meaning string) {

	name := DefaultName
	status := func(context.Context) (State, error) { return Absent, nil }

	if b.Kind == HelperVM {
		m, err := NewManager(b.Helper, os.Stderr)
		if err != nil {
			return false, "helper VM: " + err.Error(), "fix the setting it names"
		}

		name, status = m.Config.Name, m.Status
	}

	return DoctorRow(ctx, b, name, status)
}
