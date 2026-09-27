package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

// A layered VM (fcVM.Layout) boots its image's shared, read-only root filesystem with a writable
// layer of its own over it - fc/layer.go and DECISIONS.md ("A microVM links its image and writes
// to a layer of its own"). Every create since v0.14 is layered, unless SBX_FC_ROOTFS=copy or it
// starts from a disk snapshot saved before layers.
const layoutLayered = "layered"

func (vm *fcVM) layered() bool { return vm.Layout == layoutLayered }

// RootfsLayoutEnv picks how a new VM holds its root filesystem: "layered" (the default) or "copy",
// v0.12's whole copy of the image per VM - for a guest kernel without overlayfs (SBX_FC_KERNEL).
const RootfsLayoutEnv = "SBX_FC_ROOTFS"

func layeredRootfs() (bool, error) {
	switch v := os.Getenv(RootfsLayoutEnv); v {
	case "", layoutLayered:
		return true, nil
	case "copy":
		return false, nil
	default:
		return false, fmt.Errorf("%s=%q must be %q (the image shared read-only, a writable layer per VM) or "+
			"\"copy\" (a whole copy of the image per VM)", RootfsLayoutEnv, v, layoutLayered)
	}
}

// DiskSizeEnv is the size of a layered VM's writable layer, in the spec's memory syntax (10g,
// 512m): the most a VM can write to its root filesystem. Sparse, so it costs what is written.
const DiskSizeEnv = "SBX_FC_DISK_SIZE"

// defaultDiskSize is a writable layer's size when DiskSizeEnv is unset: the same as a pvc volume's.
const defaultDiskSize = 10 << 30

func diskSize() (int64, error) {
	v := os.Getenv(DiskSizeEnv)
	if v == "" {
		return defaultDiskSize, nil
	}

	n, err := parseSize(v)
	if err != nil || n < 64<<20 {
		return 0, fmt.Errorf("%s=%q must be a size of at least 64m", DiskSizeEnv, v)
	}

	return int64(n), nil
}

// makeRootfs puts the new VM's root filesystem in dir, and says what it did: the layout, how the
// file came to be there (fcVM.Clone), and a note for the create's timing line. Given baseSrc it is
// layered - the base linked, and upperSrc cloned or an empty writable layer made; given rootfsSrc,
// a whole copy of it. The caller removes dir on failure.
func (p *fcProvider) makeRootfs(ctx context.Context, dir, rootfsSrc, baseSrc, upperSrc string) (layout, clone, note string, err error) {
	if baseSrc == "" {
		clone, err = fc.CloneFile(rootfsSrc, filepath.Join(dir, fc.RootfsName))
		if err != nil {
			return "", "", "", fmt.Errorf("cloning the root filesystem: %w", err)
		}

		return "", clone, fmt.Sprintf("a whole copy of the image, by %s: %s of data", clone, cloneSize(rootfsSrc)), nil
	}

	clone, err = fc.LinkShared(baseSrc, filepath.Join(dir, fc.BaseName))
	if err != nil {
		return "", "", "", fmt.Errorf("linking the image's shared root filesystem: %w", err)
	}

	upper := filepath.Join(dir, fc.UpperName)

	if upperSrc != "" {
		how, err := fc.CloneFile(upperSrc, upper)
		if err != nil {
			return "", "", "", fmt.Errorf("cloning the snapshot's writable layer: %w", err)
		}

		return layoutLayered, clone, fmt.Sprintf("layered: the image's base by %s, %s of data not copied; "+
			"the snapshot's writable layer by %s, %s", clone, cloneSize(baseSrc), how, cloneSize(upperSrc)), nil
	}

	size, err := diskSize()
	if err != nil {
		return "", "", "", err
	}

	if err := fc.BuildUpperDrive(ctx, p.ext4, upper, size); err != nil {
		return "", "", "", err
	}

	return layoutLayered, clone, fmt.Sprintf("layered: the image's base by %s, %s of data not copied; "+
		"a new writable layer of at most %s", clone, cloneSize(baseSrc), bytesIEC(size)), nil
}

// rootfsFiles are the VM's root filesystem files in its directory, each with whether it is the
// shared base - linked (fc.LinkShared), never copied, wherever it goes - or the VM's own.
func (vm *fcVM) rootfsFiles() []vmFile {
	if vm.layered() {
		return []vmFile{{fc.BaseName, true}, {fc.UpperName, false}}
	}

	return []vmFile{{fc.RootfsName, false}}
}

type vmFile struct {
	name   string
	shared bool
}

// copyVMFile puts dir's f at dst's: a link to a shared base, a clone of anything else.
func copyVMFile(f vmFile, dir, dst string) error {
	var err error

	if f.shared {
		_, err = fc.LinkShared(filepath.Join(dir, f.name), filepath.Join(dst, f.name))
	} else {
		_, err = fc.CloneFile(filepath.Join(dir, f.name), filepath.Join(dst, f.name))
	}

	return err
}
