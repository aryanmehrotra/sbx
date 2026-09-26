package provider

// What the OpenSandbox API needs from a microVM, beyond what sandbox.json does.
//
// An API sandbox is a VM like any other, with four differences, each a decision in DECISIONS.md
// ("The OpenSandbox API on a microVM"):
//
//   - its agent is the one fc-init already makes PID 1 (RunsAgent), booted with the token the API
//     minted - never one this provider mints;
//   - it is born running, not asleep: the API's contract is a process that is running, and a
//     snapshot-then-restore at create would cost a second boot's worth of time for nothing;
//   - a snapshot of it is its disk, not its memory, and a sandbox created from one is a new VM
//     cold-booted from a copy of that disk;
//   - a pvc volume is an ext4 image attached as an extra drive.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/spec"
)

// RunsAgent: every VM's PID 1 is fc-init, which becomes execd (internal/fc/guestinit).
func (p *fcProvider) RunsAgent() {}

// ImageInfo reads an image through the engine that builds root filesystems - or, for a disk
// snapshot of an API sandbox, the image config it was saved with.
func (p *fcProvider) ImageInfo(ctx context.Context, image string) (ImageInfo, error) {
	if s, ok := p.snapshotFor(image); ok && s.DiskOnly && s.VM.Config != nil {
		c := s.VM.Config

		return ImageInfo{Entrypoint: c.Entrypoint, Cmd: c.Cmd, OS: "linux", Arch: p.arch}, nil
	}

	c, err := p.rootfs.Engine.Inspect(ctx, image)
	if err != nil {
		return ImageInfo{}, err
	}

	return ImageInfo{Entrypoint: c.Entrypoint, Cmd: c.Cmd, OS: c.OS, Arch: c.Arch}, nil
}

// Pull fetches an image into the engine that builds root filesystems.
func (p *fcProvider) Pull(ctx context.Context, image string) error {
	return p.rootfs.Engine.Pull(ctx, image)
}

// agentToken is the execd access token a VM boots with: the API's, when the spec carries one,
// and otherwise one minted here for a sandbox.json service, which has no API to mint it.
func agentToken(svc spec.Service) string {
	if t := svc.Env[AgentTokenEnv]; t != "" {
		return t
	}

	return randomHex(16)
}

// fcVolume is a named volume attached to a VM as an extra drive.
type fcVolume struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	SubPath  string `json:"sub_path,omitempty"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// fcVolumeName is what a named volume may be called: it becomes a file name.
var fcVolumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)

func (p *fcProvider) volumePath(name string) string {
	return filepath.Join(p.root, "volumes", name+".ext4")
}

// defaultVolumeSize is a new volume's size: sparse, so it costs what is written, not this. A
// docker named volume has no size at all; an ext4 image has to have one.
const defaultVolumeSize = 10 << 30

// VolumeSizeEnv overrides defaultVolumeSize, in the spec's memory syntax (10g, 512m).
const VolumeSizeEnv = "SBX_FC_VOLUME_SIZE"

func volumeSize() (int64, error) {
	v := os.Getenv(VolumeSizeEnv)
	if v == "" {
		return defaultVolumeSize, nil
	}

	n, err := parseSize(v)
	if err != nil || n < 16<<20 {
		return 0, fmt.Errorf("%s=%q must be a size of at least 16m", VolumeSizeEnv, v)
	}

	return int64(n), nil
}

// VolumeExists reports whether the volume's image exists.
func (p *fcProvider) VolumeExists(_ context.Context, name string) (bool, error) {
	if !fcVolumeName.MatchString(name) {
		return false, fmt.Errorf("%q is not a volume name", name)
	}

	_, err := os.Stat(p.volumePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return err == nil, err
}

// CreateVolume makes an empty ext4 image, sparse. Written to a temporary name and renamed, so a
// half-made image is never found by name; labels are kept beside it for whoever lists them.
func (p *fcProvider) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	if !fcVolumeName.MatchString(name) {
		return fmt.Errorf("%q is not a volume name", name)
	}

	size, err := volumeSize()
	if err != nil {
		return err
	}

	// Under the lock a VM create holds from its volume check to its save: two creates of one name
	// must not both pass the "exists" check below and write over each other's image.
	p.volMu.Lock()
	defer p.volMu.Unlock()

	dir := filepath.Join(p.root, "volumes")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	path := p.volumePath(name)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("volume %s already exists", name)
	}

	empty, err := os.MkdirTemp(dir, ".empty-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(empty)

	tmp := path + ".new"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}

	err = f.Truncate(size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err == nil {
		err = p.ext4.Build(ctx, empty, tmp, "sbx-volume")
	}

	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("making volume %s: %w", name, err)
	}

	if b, err := json.Marshal(map[string]any{"labels": labels, "created": time.Now().UTC()}); err == nil {
		_ = os.WriteFile(strings.TrimSuffix(path, ".ext4")+".json", b, 0o600)
	}

	return os.Rename(tmp, path)
}

// RemoveVolume deletes a volume no VM has attached. One that is attached is refused, as docker
// refuses a volume a container mounts: a sleeping VM's snapshot names the file, and its next
// wake would fail on a disk that is gone.
func (p *fcProvider) RemoveVolume(_ context.Context, name string) error {
	if !fcVolumeName.MatchString(name) {
		return fmt.Errorf("%q is not a volume name", name)
	}

	// Held through the delete: a VM create between its check that this volume is free and the
	// save that attaches it would otherwise lose the volume under it.
	p.volMu.Lock()
	defer p.volMu.Unlock()

	if by := p.attachedTo(name, ""); by != "" {
		return fmt.Errorf("volume %s is attached to %s; remove that first", name, by)
	}

	err := os.Remove(p.volumePath(name))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}

	_ = os.Remove(strings.TrimSuffix(p.volumePath(name), ".ext4") + ".json")

	return err
}

// attachedTo is the ref of a VM other than except that has name attached, or "".
func (p *fcProvider) attachedTo(name, except string) string {
	vms, _ := p.all()

	for _, vm := range vms {
		if vm.Ref == except {
			continue
		}

		for _, v := range vm.Volumes {
			if v.Name == name {
				return vm.Ref
			}
		}
	}

	return ""
}

// volumesFor turns a spec's named-volume mounts into this VM's drives, checking each exists and
// is attached nowhere else. A drive is one ext4 filesystem, and two kernels mounting it - two
// VMs, or one VM twice - corrupt it, which docker's shared named volumes never had to consider.
// The caller holds p.volMu until the VM's record is saved, so two creates cannot both pass.
func (p *fcProvider) volumesFor(ref string, mounts []spec.VolumeMount) ([]fcVolume, error) {
	var out []fcVolume

	seen := map[string]bool{}

	for _, m := range mounts {
		if m.Volume == "" {
			continue // a host mount; unsupported() has refused it already
		}

		if seen[m.Volume] {
			return nil, fmt.Errorf("volume %s is mounted twice: on a microVM a volume is a block device, "+
				"which one kernel mounts once - mount it at one path (a sub_path of it is fine)", m.Volume)
		}

		seen[m.Volume] = true

		ok, err := p.VolumeExists(context.Background(), m.Volume)
		if err != nil {
			return nil, err
		}

		if !ok {
			return nil, fmt.Errorf("no volume %s (a microVM volume is an ext4 image under %s)", m.Volume,
				filepath.Join(p.root, "volumes"))
		}

		if by := p.attachedTo(m.Volume, ref); by != "" {
			return nil, fmt.Errorf("volume %s is attached to %s: on a microVM a volume is a block device, "+
				"and two VMs mounting one ext4 filesystem would corrupt it - it can be used by one "+
				"sandbox at a time", m.Volume, by)
		}

		out = append(out, fcVolume{Name: m.Volume, Target: m.Target, SubPath: m.SubPath, ReadOnly: m.ReadOnly})
	}

	if len(out) > fc.MaxExtraDrives {
		return nil, fmt.Errorf("%d volumes: a microVM takes at most %d", len(out), fc.MaxExtraDrives)
	}

	return out, nil
}

// initMounts is what fc-init mounts for the VM's volumes, in drive order.
func initMounts(vs []fcVolume) []fc.InitMount {
	var out []fc.InitMount

	for i, v := range vs {
		out = append(out, fc.InitMount{Device: fc.GuestExtraDevice(i), Target: v.Target, SubPath: v.SubPath,
			ReadOnly: v.ReadOnly})
	}

	return out
}

// volumeDriveID is the i-th volume's drive id in Firecracker's API.
func volumeDriveID(i int) string { return fmt.Sprintf("vol%d", i) }

// commitDisk saves an API sandbox's disk - not its memory - as image: the decision in
// DECISIONS.md ("An API snapshot of a microVM is its disk"). A sandbox created from it is a new VM
// cold-booted from a copy, with its own agent drive, token, address and identity; nothing the
// source's memory held (its keys, its RNG state, its IP) is in it.
//
// A running VM is asked to sync first, through execd - the agent binary's own `fssync`, so it
// works in an image with no `sync` - and paused while its disk is copied, so what a caller wrote
// a moment before is on the copy and nothing is written during it. The caller holds the lock.
func (p *fcProvider) commitDisk(ctx context.Context, vm *fcVM, state, dst string) error {
	dir := p.dir(vm.Ref)
	c := p.client(vm.Ref)

	copyDisk := func() error {
		_, err := fc.CloneFile(filepath.Join(dir, fc.RootfsName), filepath.Join(dst, fc.RootfsName))
		return err
	}

	switch state {
	case fc.StateRunning, fc.StatePaused:
		if state == fc.StatePaused {
			if err := c.Resume(ctx); err != nil {
				return err
			}
		}

		if p.guest.Available() {
			sctx, cancel := context.WithTimeout(ctx, sealTimeout)
			_, err := p.execAs(sctx, vm, []string{fc.GuestAgentPath, "fssync"})
			cancel()

			if err != nil {
				if state == fc.StatePaused {
					_ = c.Pause(context.WithoutCancel(ctx))
				}

				return fmt.Errorf("syncing the guest's filesystems before copying its disk: %w", err)
			}
		}

		if err := c.Pause(ctx); err != nil {
			return err
		}

		err := copyDisk()

		// Left as it was found: frozen if it was frozen.
		if state == fc.StateRunning {
			if rerr := c.Resume(context.WithoutCancel(ctx)); rerr != nil {
				return errors.Join(err, rerr)
			}
		}

		return err
	default:
		// Asleep: the disk is as the last sleep left it (execd synced before that snapshot), or,
		// for a VM that died awake, as a power loss left it, which its journal recovers from.
		return copyDisk()
	}
}

// execAs runs argv in vm through execd with the token vm holds.
func (p *fcProvider) execAs(ctx context.Context, vm *fcVM, argv []string) (string, error) {
	run := p.healthExec
	if run == nil {
		run = p.Exec
	}

	return run(ctx, vm.Ref, argv)
}

// forgetSecrets is a record fit to be saved with a disk snapshot: no identity of the source's.
func forgetSecrets(vm fcVM) fcVM {
	vm.AccessToken, vm.ControlSecret, vm.LiveSecret, vm.PendingSecret = "", "", "", ""
	vm.BootToken, vm.ClaimEnv, vm.Pooled = "", nil, false
	vm.Volumes = nil

	return vm
}
