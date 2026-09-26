package provider

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

// The warm pool under the jailer (v0.13 merges both): a member is made, parked and claimed
// through its jail like any VM - every launch jailed as the uid of its address, the parked
// snapshot written in the root and taken back out, the claim's restore staged into a fresh root,
// and the re-key reaching execd through <dir>/vsock.sock, the symlink into that root.

func osbJailRig(t *testing.T) *rig {
	t.Helper()

	r, _ := osbRig(t)
	r.p.jail = &fc.JailConfig{UIDBase: fc.DefaultJailUIDBase}
	r.p.jailer = func(context.Context) (string, error) { return "/pinned/jailer", nil }

	return r
}

func TestAJailedAsleepMemberIsParkedAndClaimedThroughItsJail(t *testing.T) {
	r := osbJailRig(t)

	ref := r.create(t, "osb-jpool1", apiSvc("member-token"))
	dir := r.p.dir(ref)

	if err := r.p.Park(r.ctx, ref, false); err != nil {
		t.Fatal(err)
	}

	vm := r.vm(t, ref)

	// The documented scheme (SECURITY.md: 900000 + slot*256 + index), computed here rather than by
	// the code under test, so a change to the arithmetic fails this test instead of moving with it.
	uid := 900000 + vm.Slot*256 + vm.Index
	if vm.JailUID != uid {
		t.Fatalf("the record says its VMM was jailed as uid %d, want %d", vm.JailUID, uid)
	}

	for i, s := range r.l.specs {
		if s.Jail == nil || s.Jail.UID != uid || s.Jail.UID == 0 {
			t.Fatalf("launch %d of the member was not jailed as uid %d: %+v", i, uid, s.Jail)
		}
	}

	// Parked asleep: the Full snapshot was written in the root and taken back to the VM's dir,
	// and the record says its snapshot names paths in a jail.
	parkCalls := r.l.history[len(r.l.history)-1].Calls()
	if got := body(parkCalls, "PUT /snapshot/create", "mem_file_path"); got != "/"+fc.MemName+".new" {
		t.Fatalf("the parked member's memory was written at %q, not in its jail", got)
	}

	if !vm.Pooled || !vm.SnapshotValid || !vm.SnapshotJailed {
		t.Fatalf("parked record: pooled %v valid %v jailed %v", vm.Pooled, vm.SnapshotValid, vm.SnapshotJailed)
	}

	if _, err := os.Stat(filepath.Join(dir, fc.MemName)); err != nil {
		t.Fatalf("the parked memory file is not in the VM's directory: %v", err)
	}

	launches := len(r.l.specs)

	if err := r.p.Claim(r.ctx, ref, "caller-token", map[string]string{"B": "2"}); err != nil {
		t.Fatal(err)
	}

	if len(r.l.specs) != launches+1 {
		t.Fatalf("the claim launched %d VMMs, want 1 restore", len(r.l.specs)-launches)
	}

	claim := r.l.specs[len(r.l.specs)-1]
	if claim.Jail == nil || claim.Jail.UID != uid {
		t.Fatalf("the claim's restore was not jailed as uid %d: %+v", uid, claim.Jail)
	}

	if names := stagedNames(claim); !slices.Equal(names, []string{"agent.ext4", "rootfs.ext4", "vm.mem", "vm.state"}) {
		t.Fatalf("the claim's restore staged %v", names)
	}

	load := r.l.server(dir).Calls()
	if got := body(load, "PUT /snapshot/load", "snapshot_path"); got != "/"+fc.StateName {
		t.Fatalf("the claim loaded %q, not the snapshot in its jail", got)
	}

	var override string
	for _, c := range load {
		if c.Method+" "+c.Path == "PUT /snapshot/load" {
			if v, ok := c.Body["vsock_override"].(map[string]any); ok {
				override, _ = v["uds_path"].(string)
			}
		}
	}

	if override != "/"+fc.VsockName {
		t.Fatalf("the restored vsock device is at %q in the jail, want /%s", override, fc.VsockName)
	}

	// The re-key dials <dir>/vsock.sock, which must lead into this launch's root: the device the
	// jailed VMM bound at /vsock.sock.
	root := fc.JailRoot(dir, vm.Binary)
	if got, err := os.Readlink(filepath.Join(dir, fc.VsockName)); err != nil || got != filepath.Join(root, fc.VsockName) {
		t.Fatalf("%s -> %q (%v), want the jail's %s", fc.VsockName, got, err, filepath.Join(root, fc.VsockName))
	}

	if k, state := r.lastRekey(t); k.AccessToken != "caller-token" || state != fc.StateRunning {
		t.Fatalf("re-key %+v in state %q, want the caller's token on the restored VM", k, state)
	}

	// The claimed VM's disks in its root are the VM's own files (the rootfs as the extents clone
	// made it), not copies: what the guest writes is on the VM's disk.
	for _, f := range []string{fc.RootfsName, "agent.ext4"} {
		a, err1 := os.Stat(filepath.Join(dir, f))
		j, err2 := os.Stat(filepath.Join(root, f))

		if err1 != nil || err2 != nil || !os.SameFile(a, j) {
			t.Fatalf("the claimed member's /%s is not its own disk (%v, %v)", f, err1, err2)
		}
	}
}

func TestAJailedFrozenMemberIsResumedAndReKeyed(t *testing.T) {
	r := osbJailRig(t)

	ref := r.create(t, "osb-jpool2", apiSvc("member-token"))
	dir := r.p.dir(ref)

	if err := r.p.Park(r.ctx, ref, true); err != nil {
		t.Fatal(err)
	}

	if s := r.l.server(dir); s == nil || s.State() != fc.StatePaused {
		t.Fatal("a jailed frozen member is not paused")
	}

	launches := len(r.l.specs)
	if last := r.l.specs[launches-1]; last.Jail == nil || last.Jail.UID != 900000+r.vm(t, ref).Slot*256+r.vm(t, ref).Index {
		t.Fatalf("the frozen member's VMM is not jailed: %+v", last.Jail)
	}

	if err := r.p.Claim(r.ctx, ref, "caller-token", nil); err != nil {
		t.Fatal(err)
	}

	if len(r.l.specs) != launches {
		t.Fatal("a frozen claim launched a VMM; it must resume the jailed one it has")
	}

	if k, state := r.lastRekey(t); k.AccessToken != "caller-token" || state != fc.StateRunning {
		t.Fatalf("re-key %+v in state %q", k, state)
	}
}

// A member parked with the jailer one way and claimed with it the other has a snapshot naming
// drive paths its VMM cannot open. It is no longer warm: the claim refuses it (the API discards
// it and goes to the next member or cold) and never loads that snapshot - exactly as Start
// refuses to restore it.
func TestAMemberParkedInTheOtherJailModeIsNotClaimed(t *testing.T) {
	for _, jailedFirst := range []bool{true, false} {
		r, _ := osbRig(t)
		r.p.jailer = func(context.Context) (string, error) { return "/pinned/jailer", nil }

		if jailedFirst {
			r.p.jail = &fc.JailConfig{UIDBase: fc.DefaultJailUIDBase}
		}

		ref := r.create(t, "osb-jpool3", apiSvc("member-token"))

		if err := r.p.Park(r.ctx, ref, false); err != nil {
			t.Fatal(err)
		}

		if jailedFirst {
			r.p.jail = nil
		} else {
			r.p.jail = &fc.JailConfig{UIDBase: fc.DefaultJailUIDBase}
		}

		launches := len(r.l.specs)

		if err := r.p.Claim(r.ctx, ref, "caller-token", nil); err == nil {
			t.Fatalf("jailed first %v: a member whose snapshot the VMM cannot open was claimed", jailedFirst)
		}

		if len(r.l.specs) != launches {
			t.Fatalf("jailed first %v: the claim launched a VMM to load a snapshot it cannot open", jailedFirst)
		}

		if vm := r.vm(t, ref); vm.AccessToken == "caller-token" {
			t.Fatalf("jailed first %v: a refused claim still gave the member the caller's token", jailedFirst)
		}
	}
}
