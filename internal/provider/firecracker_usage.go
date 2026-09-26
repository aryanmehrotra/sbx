package provider

import (
	"encoding/json"
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

	// PoolMembers are VMs parked as OpenSandbox warm-pool members, and PoolMemory the part of
	// Memory they hold: a member waiting asleep costs no RAM and a memory file as big as its RAM.
	PoolMembers int
	PoolMemory  int64
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

		var mem int64
		for _, f := range []string{fc.MemName, fc.DiffMemName, fc.StateName} {
			mem += allocated(filepath.Join(dir, f))
		}

		u.Memory += mem

		if parked(dir) {
			u.PoolMembers++
			u.PoolMemory += mem
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

// parked reports whether the VM in dir is a warm-pool member waiting for a claim. A record that
// cannot be read is not one: this is a report, and a guess would inflate it.
func parked(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, "vm.json"))
	if err != nil {
		return false
	}

	var rec struct {
		Pooled bool `json:"pooled"`
	}

	return json.Unmarshal(b, &rec) == nil && rec.Pooled
}
