package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

// initRecorder is an Ext4Builder that keeps every agent drive's /init.json, by the image it
// was built into, so a test reads what fc-init would.
type initRecorder struct {
	touchExt4

	mu    sync.Mutex
	inits map[string]fc.InitConfig
}

func (r *initRecorder) Build(ctx context.Context, src, img, label string) error {
	if b, err := os.ReadFile(filepath.Join(src, "init.json")); err == nil {
		var cfg fc.InitConfig
		if json.Unmarshal(b, &cfg) == nil {
			r.mu.Lock()
			r.inits[img] = cfg
			r.mu.Unlock()
		}
	}

	return r.touchExt4.Build(ctx, src, img, label)
}

func (r *initRecorder) initOf(img string) (fc.InitConfig, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cfg, ok := r.inits[img]

	return cfg, ok
}

// driveBody is the last PUT /drives/<id> a VM's VMM was given.
func driveBody(r *rig, dir, id string) map[string]any {
	s := r.l.server(dir)
	if s == nil {
		return nil
	}

	var body map[string]any

	for _, c := range s.Calls() {
		if c.Method == "PUT" && c.Path == "/drives/"+id {
			body = c.Body
		}
	}

	return body
}

// Known issue of v0.12 (K4): without reflink every VM copied its image's whole root filesystem -
// about 16 s for the 7.4 GiB code-interpreter image in CI. Now no VM copies it: each links the
// ONE read-only file the image was built into (the same inode, so no data is read or written),
// attaches it read-only, and gets a small writable drive of its own that fc-init lays over it
// with overlayfs. Two VMs of one image share the base and nothing else.
func TestAVMLinksItsImagesSharedBaseAndWritesOnlyItsOwnLayer(t *testing.T) {
	r := newRig(t)
	rec := &initRecorder{inits: map[string]fc.InitConfig{}}
	r.p.ext4 = rec

	api := redis
	api.OSBOwner = "osb-a"

	refs := []string{}

	for _, sb := range []string{"k4a", "k4b"} {
		refs = append(refs, r.create(t, sb, api))
	}

	rfs, err := r.p.rootfs.Build(r.ctx, redis.Image)
	if err != nil {
		t.Fatal(err)
	}

	cache, err := os.Stat(rfs.Path)
	if err != nil {
		t.Fatal(err)
	}

	for _, ref := range refs {
		dir := r.p.dir(ref)

		if _, err := os.Stat(filepath.Join(dir, "rootfs.ext4")); err == nil {
			t.Fatalf("%s holds a per-VM copy of the image's root filesystem", ref)
		}

		base, err := os.Stat(filepath.Join(dir, "base.ext4"))
		if err != nil {
			t.Fatalf("%s has no base drive: %v", ref, err)
		}

		if !os.SameFile(base, cache) {
			t.Fatalf("%s's base is not the image's cached root filesystem (a copy, not a link)", ref)
		}

		if base.Mode().Perm()&0o222 != 0 {
			t.Fatalf("the shared base is writable (%v): one VM's VMM could change every VM's image", base.Mode())
		}

		upper, err := os.Stat(filepath.Join(dir, "upper.ext4"))
		if err != nil {
			t.Fatalf("%s has no writable layer: %v", ref, err)
		}

		if os.SameFile(upper, cache) {
			t.Fatal("the writable layer is the shared base")
		}

		if d := driveBody(r, dir, "rootfs"); d == nil || d["is_read_only"] != true ||
			filepath.Base(d["path_on_host"].(string)) != "base.ext4" {
			t.Fatalf("%s's root drive = %v, want base.ext4, read-only", ref, d)
		}

		if d := driveBody(r, dir, "upper"); d == nil || d["is_read_only"] == true ||
			filepath.Base(d["path_on_host"].(string)) != "upper.ext4" {
			t.Fatalf("%s's writable drive = %v, want upper.ext4, read-write", ref, d)
		}

		init, ok := rec.initOf(filepath.Join(dir, "agent.ext4"))
		if !ok {
			t.Fatalf("%s: no agent drive was built", ref)
		}

		if init.RootDevice != "/dev/vdb" || init.UpperDevice != "/dev/vdc" {
			t.Fatalf("%s: fc-init mounts root %q over upper %q, want /dev/vdb under /dev/vdc",
				ref, init.RootDevice, init.UpperDevice)
		}
	}
}

func stat(t *testing.T, path string) os.FileInfo {
	t.Helper()

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return st
}

// SBX_FC_ROOTFS=copy is v0.12's layout, for a guest kernel without overlayfs: a whole copy of the
// image, read-write, and no writable layer. A VM recorded that way (every VM made before v0.13)
// keeps booting that way whatever the variable says now - its snapshot names its drives.
func TestTheCopyLayoutIsKeptForTheVMsThatHaveIt(t *testing.T) {
	r := newRig(t)
	t.Setenv(RootfsLayoutEnv, "copy")

	ref := r.create(t, "k4c", redis)
	dir := r.p.dir(ref)

	if vm := r.vm(t, ref); vm.layered() {
		t.Fatalf("%s=copy made a layered VM: %+v", RootfsLayoutEnv, vm)
	}

	for f, want := range map[string]bool{fc.RootfsName: true, fc.BaseName: false, fc.UpperName: false} {
		if _, err := os.Stat(filepath.Join(dir, f)); (err == nil) != want {
			t.Fatalf("%s present = %v, want %v", f, err == nil, want)
		}
	}

	t.Setenv(RootfsLayoutEnv, "")

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	// A cold boot re-issues the drives: still its own copy, read-write.
	vm := r.vm(t, ref)
	vm.SnapshotValid = false

	if err := r.p.save(vm); err != nil {
		t.Fatal(err)
	}

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if d := driveBody(r, dir, "rootfs"); d == nil || d["is_read_only"] == true ||
		filepath.Base(d["path_on_host"].(string)) != fc.RootfsName {
		t.Fatalf("a copy-layout VM's cold boot attached %v", d)
	}

	if d := driveBody(r, dir, "upper"); d != nil {
		t.Fatalf("a copy-layout VM was given a writable layer: %v", d)
	}

	t.Setenv(RootfsLayoutEnv, "overlay-please")

	if err := r.p.Create(r.ctx, "k4d", 3, 0, "cache", redis, nil, "", IsolationContainer); err == nil {
		t.Fatalf("%s=overlay-please was accepted", RootfsLayoutEnv)
	}
}

// The writable layer is as big as SBX_FC_DISK_SIZE says, and no bigger: it is the most one VM can
// write to its root filesystem on the host's disk.
func TestTheWritableLayerIsTheDiskSize(t *testing.T) {
	r := newRig(t)
	r.p.ext4 = sizeKeepingExt4{}

	for env, want := range map[string]int64{"": defaultDiskSize, "3g": 3 << 30} {
		t.Setenv(DiskSizeEnv, env)

		ref := r.create(t, "k6"+map[string]string{"": "d", "3g": "e"}[env], redis)

		if got := stat(t, filepath.Join(r.p.dir(ref), fc.UpperName)).Size(); got != want {
			t.Fatalf("%s=%q: writable layer %d bytes, want %d", DiskSizeEnv, env, got, want)
		}
	}

	t.Setenv(DiskSizeEnv, "1m")

	if _, err := diskSize(); err == nil {
		t.Fatalf("%s=1m accepted: too small to hold an ext4 journal", DiskSizeEnv)
	}
}

// sizeKeepingExt4 formats in place, as mkfs.ext4 does: the file keeps the size it was given
// (touchExt4 rewrites it to four bytes).
type sizeKeepingExt4 struct{ touchExt4 }

func (sizeKeepingExt4) Build(_ context.Context, _, img, _ string) error {
	if _, err := os.Stat(img); err == nil {
		return nil
	}

	return os.WriteFile(img, []byte("ext4"), 0o600)
}
