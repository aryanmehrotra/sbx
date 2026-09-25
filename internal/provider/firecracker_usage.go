package provider

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

// FirecrackerUsage is what the firecracker provider holds on disk, in bytes actually allocated:
// a sleeping VM's memory file is as big as its RAM, and an idle fleet of them is the provider's
// real cost, so `sbx doctor` says how much it is.
type FirecrackerUsage struct {
	Root      string
	VMs       int
	Memory    int64 // vm.mem, diff.mem and vm.state of every VM
	Disks     int64 // every VM's root filesystem and agent drive
	Snapshots int64 // everything under snapshots/
}

// Total is every byte counted.
func (u FirecrackerUsage) Total() int64 { return u.Memory + u.Disks + u.Snapshots }

// FirecrackerDiskUsage walks the provider's state directory (SBX_FC_STATE, else ~/.sbx/fc). A
// state directory that does not exist is zero, not an error: nothing was ever created here.
func FirecrackerDiskUsage() (FirecrackerUsage, error) {
	root, err := stateRoot()
	if err != nil {
		return FirecrackerUsage{}, err
	}

	u := FirecrackerUsage{Root: root}

	vms, err := os.ReadDir(filepath.Join(root, "vms"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return u, err
	}

	for _, d := range vms {
		if !d.IsDir() {
			continue
		}

		u.VMs++

		dir := filepath.Join(root, "vms", d.Name())

		for _, f := range []string{fc.MemName, fc.DiffMemName, fc.StateName} {
			u.Memory += allocated(filepath.Join(dir, f))
		}

		for _, f := range []string{fc.RootfsName, "agent.ext4"} {
			u.Disks += allocated(filepath.Join(dir, f))
		}
	}

	err = filepath.WalkDir(filepath.Join(root, "snapshots"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		if d.Type().IsRegular() {
			u.Snapshots += allocated(p)
		}

		return nil
	})

	return u, err
}
