package provider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	Volumes   int64 // every pvc's ext4 image under volumes/

	// Jails is what VMM jails hold that nothing above counts: a file there whose only name is in the
	// jail - a per-VM copy of a kernel that could not be shared, a link left to a replaced snapshot
	// by a VMM that has not been released. A jail's links to the VM's own drives and to the shared
	// kernel have another name, and are counted there (or are the artifact cache's).
	Jails int64

	// PoolMembers are VMs parked as OpenSandbox warm-pool members, and PoolMemory the part of
	// Memory they hold: a member waiting asleep costs no RAM and a memory file as big as its RAM.
	PoolMembers int
	PoolMemory  int64
}

// Total is every byte counted.
func (u FirecrackerUsage) Total() int64 {
	return u.Memory + u.Disks + u.Snapshots + u.Volumes + u.Jails
}

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

		jails, err := onlyNamedUnder(filepath.Join(dir, fc.JailDirName))
		if err != nil {
			return u, err
		}

		u.Jails += jails
	}

	u.Snapshots, err = allocatedUnder(filepath.Join(root, "snapshots"))
	if err != nil {
		return u, err
	}

	u.Volumes, err = allocatedUnder(filepath.Join(root, "volumes"))

	return u, err
}

// allocatedUnder is what every regular file under dir holds on disk; a dir that does not exist
// holds nothing.
func allocatedUnder(dir string) (int64, error) {
	var n int64

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		if d.Type().IsRegular() {
			n += allocated(p)
		}

		return nil
	})

	return n, err
}

// onlyNamedUnder is what the regular files under dir hold on disk that no other name holds: a file
// with one link is this tree's alone; one with more is a link to something counted elsewhere (a
// VM's drive) or owned elsewhere (the shared kernel).
func onlyNamedUnder(dir string) (int64, error) {
	var n int64

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		if d.Type().IsRegular() && links(p) == 1 {
			n += allocated(p)
		}

		return nil
	})

	return n, err
}

// cloneSize is what a rootfs holds on disk, for the create log: a copy costs this, not the
// file's apparent size.
func cloneSize(path string) string { return bytesIEC(allocated(path)) }

func bytesIEC(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KiB", n>>10)
	}
}

// KeepConsolesEnv names a directory a removed VM's console is copied into first - for CI, where
// the suite deletes each sandbox it made, and a failure's only evidence is what its guest printed.
// Unset (the default) keeps nothing.
const KeepConsolesEnv = "SBX_FC_KEEP_CONSOLES"

// keepConsole copies the last MiB of dir's console.log and vmm.log to $SBX_FC_KEEP_CONSOLES, named
// for the VM. Best effort: it is evidence, not state.
func keepConsole(dir string, vm *fcVM) {
	to := os.Getenv(KeepConsolesEnv)
	if to == "" {
		return
	}

	if err := os.MkdirAll(to, 0o755); err != nil {
		return
	}

	for _, f := range []string{fc.ConsoleName, fc.VMMLogName} {
		var b bytes.Buffer
		if tailBytes(filepath.Join(dir, f), 1<<20, &b) != nil {
			continue
		}

		_ = os.WriteFile(filepath.Join(to, fmt.Sprintf("%s-%s-%s.%s", vm.Sandbox, vm.Service, vm.Instance, f)),
			b.Bytes(), 0o644)
	}
}

func tailBytes(path string, n int64, w *bytes.Buffer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}

	_, err = f.Seek(max(0, st.Size()-n), 0)
	if err == nil {
		_, err = w.ReadFrom(f)
	}

	return err
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
