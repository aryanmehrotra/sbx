package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/aryanmehrotra/sbx/internal/fc"
	"github.com/aryanmehrotra/sbx/internal/fc/fcfake"
)

// The provider under the jailer, against fcfake running with Root set: every path it hands the
// VMM must resolve inside the jail, every file it names must have been staged there, and every
// file the VMM writes must come back to the VM's directory. What this cannot show (the real
// jailer's chroot, uid and cgroup) is CI's microvm job.

func jailRig(t *testing.T) *rig {
	t.Helper()

	r := newRig(t)
	r.p.jail = &fc.JailConfig{UIDBase: fc.DefaultJailUIDBase}
	r.p.jailer = func(context.Context) (string, error) { return "/pinned/jailer", nil }

	return r
}

func body(calls []fcfake.Call, key, field string) string {
	for _, c := range calls {
		if c.Method+" "+c.Path == key {
			s, _ := c.Body[field].(string)
			return s
		}
	}

	return ""
}

func TestAJailedVMIsGivenOnlyPathsInsideItsRoot(t *testing.T) {
	r := jailRig(t)
	r.g.available = false

	ref := r.create(t, "j1", redis)
	dir := r.p.dir(ref)
	vm := r.vm(t, ref)

	specs := r.l.specs
	if len(specs) != 1 || specs[0].Jail == nil {
		t.Fatalf("launches = %+v, want one through the jailer", specs)
	}

	j := specs[0].Jail
	if j.Jailer != "/pinned/jailer" || j.UID != r.p.jail.UID(vm.addr()) || j.GID != j.UID || j.UID == 0 {
		t.Fatalf("jail = %+v, want the pinned jailer as uid %d", j, r.p.jail.UID(vm.addr()))
	}

	if j.CPUs != vm.VCPU || j.MemMiB != vm.MemMiB {
		t.Fatalf("cgroup limits %d cpus %d MiB, want the spec's %d/%d", j.CPUs, j.MemMiB, vm.VCPU, vm.MemMiB)
	}

	var names []string
	for _, f := range j.Files {
		names = append(names, f.Name)
	}

	slices.Sort(names)

	if !slices.Equal(names, []string{"agent.ext4", "rootfs.ext4", "vmlinux"}) {
		t.Fatalf("staged %v", names)
	}

	calls := r.l.history[0].Calls()

	for key, want := range map[[2]string]string{
		{"PUT /boot-source", "kernel_image_path"}: "/vmlinux",
		{"PUT /drives/agent", "path_on_host"}:     "/agent.ext4",
		{"PUT /drives/rootfs", "path_on_host"}:    "/rootfs.ext4",
		{"PUT /vsock", "uds_path"}:                "/vsock.sock",
		{"PUT /snapshot/create", "snapshot_path"}: "/" + fc.StateName + ".new",
		{"PUT /snapshot/create", "mem_file_path"}: "/" + fc.MemName + ".new",
	} {
		if got := body(calls, key[0], key[1]); got != want {
			t.Fatalf("%s %s = %q, want %q", key[0], key[1], got, want)
		}
	}

	// What the VMM wrote in its root is the VM's snapshot now, and the jail went with the VMM.
	for _, f := range []string{fc.StateName, fc.MemName} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err != nil || len(b) == 0 {
			t.Fatalf("%s was not taken back from the jail: %v", f, err)
		}
	}

	if _, err := os.Lstat(filepath.Join(dir, fc.JailDirName)); !os.IsNotExist(err) {
		t.Fatal("the jail outlived the VMM that slept")
	}

	if !vm.SnapshotJailed {
		t.Fatal("the record does not say its snapshot names paths in a jail")
	}

	// A wake restores from the staged snapshot, and a sleep of a restored VM is a Diff taken
	// back and merged.
	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	load := r.l.server(dir).Calls()
	if got := body(load, "PUT /snapshot/load", "snapshot_path"); got != "/"+fc.StateName {
		t.Fatalf("load snapshot_path = %q", got)
	}

	if names := stagedNames(r.l.specs[1]); !slices.Equal(names, []string{"agent.ext4", "rootfs.ext4", "vm.mem", "vm.state"}) {
		t.Fatalf("a restore staged %v", names)
	}

	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if r.merge != 1 {
		t.Fatalf("merges = %d: the Diff was not taken back and merged", r.merge)
	}

	if _, err := os.Stat(filepath.Join(dir, fc.DiffMemName)); !os.IsNotExist(err) {
		t.Fatalf("diff.mem left after the merge: %v", err)
	}
}

func stagedNames(s fc.LaunchSpec) []string {
	var out []string
	for _, f := range s.Jail.Files {
		out = append(out, f.Name)
	}

	slices.Sort(out)

	return out
}

// A snapshot names its drives by the paths the VMM had: host paths unjailed, root paths jailed.
// One taken the other way cannot be loaded, so the wake is a cold boot from the disk, said.
func TestASnapshotFromTheOtherJailModeColdBoots(t *testing.T) {
	for _, jailedFirst := range []bool{true, false} {
		r := newRig(t)
		r.p.jailer = func(context.Context) (string, error) { return "/pinned/jailer", nil }

		if jailedFirst {
			r.p.jail = &fc.JailConfig{UIDBase: fc.DefaultJailUIDBase}
		}

		ref := r.create(t, "j2", redis)

		if jailedFirst {
			r.p.jail = nil
		} else {
			r.p.jail = &fc.JailConfig{UIDBase: fc.DefaultJailUIDBase}
		}

		if err := r.p.Start(r.ctx, ref); err != nil {
			t.Fatalf("jailed first %v: %v", jailedFirst, err)
		}

		if slices.Contains(pathsOf(r.l.server(r.p.dir(ref)).Calls()), "PUT /snapshot/load") {
			t.Fatalf("jailed first %v: loaded a snapshot whose drive paths the VMM cannot open", jailedFirst)
		}
	}
}

func TestACommitOfAJailedVMTakesTheSnapshotOutOfTheJail(t *testing.T) {
	r := jailRig(t)
	ref := r.create(t, "j3", redis)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	const img = "sbx-snap-j3-cache:latest"
	if err := r.p.Commit(r.ctx, ref, img); err != nil {
		t.Fatal(err)
	}

	for _, f := range []string{fc.StateName, fc.MemName, fc.RootfsName, "agent.ext4"} {
		if _, err := os.Stat(filepath.Join(r.p.snapshotDir(img), f)); err != nil {
			t.Fatalf("snapshot lacks %s: %v", f, err)
		}
	}

	calls := r.l.server(r.p.dir(ref)).Calls()
	if got := body(calls, "PUT /snapshot/create", "snapshot_path"); strings.HasPrefix(got, r.root) {
		t.Fatalf("a jailed VMM was told to write a host path: %q", got)
	}
}

// Managed (the default), a host that cannot install the guard gets no microVM at all - refused
// before anything is built. Unmanaged, the operator's firewall owns the host and it proceeds.
func TestCreateIsRefusedWhereTheHostGuardCannotRun(t *testing.T) {
	r := newRig(t)
	r.p.guardCheck = func() error { return fc.ErrNoFirewall }

	slot, _ := r.p.AllocSlot(r.ctx, "g1")
	err := r.p.Create(r.ctx, "g1", slot, 0, "cache", redis, r.p.Endpoints("g1", "cache", slot, 0, redis.Ports), "", IsolationContainer)

	if !errors.Is(err, fc.ErrNoFirewall) || !strings.Contains(err.Error(), fc.FirewallEnv+"=unmanaged") {
		t.Fatalf("Create = %v, want a refusal naming the override", err)
	}

	if r.l.launches != 0 {
		t.Fatal("a VMM was started on a host that could not guard it")
	}

	r.p.firewall = fc.FirewallUnmanaged
	r.create(t, "g1", redis)
}

// Unmanaged, the operator has declared the host firewall theirs: no rule of sbx's is missing, so
// the API caller is not told the host is open because iptables is absent.
func TestAnUnmanagedHostReportsNoMissingGuard(t *testing.T) {
	r := newRig(t)
	r.p.firewall = fc.FirewallUnmanaged
	r.create(t, "u1", redis)

	r.p.guardCheck = func() error { return fc.ErrNoFirewall }
	if w := r.p.HostWarnings(r.ctx, "u1"); len(w) != 0 {
		t.Fatalf("unmanaged: %q", w)
	}

	r.p.firewall = fc.FirewallManaged
	if w := r.p.HostWarnings(r.ctx, "u1"); len(w) != 1 {
		t.Fatalf("managed without iptables: %q", w)
	}
}

// The jail mode of a running VMM is its own, recorded when it was launched - not whatever this
// process's SBX_FC_JAILER says now. An unjailed VMM (a v0.12 record, which has no jail_uid at all)
// slept by a daemon with the jailer on must be told host paths; told "/vm.state.new" it writes a
// RAM-sized file into the host's /. And a jailed one slept with the jailer switched off must still
// be told paths in its root, and have them taken back out.
func TestTheJailModeOfARunningVMMIsTheOneItWasLaunchedWith(t *testing.T) {
	for _, jailedAtLaunch := range []bool{true, false} {
		r := newRig(t)
		r.p.jailer = func(context.Context) (string, error) { return "/pinned/jailer", nil }
		cfg := &fc.JailConfig{UIDBase: fc.DefaultJailUIDBase}

		if jailedAtLaunch {
			r.p.jail = cfg
		}

		ref := r.create(t, "jm", redis)
		dir := r.p.dir(ref)

		if err := r.p.Start(r.ctx, ref); err != nil {
			t.Fatal(err)
		}

		launched := r.l.specs[len(r.l.specs)-1]

		// The daemon restarts with the jailer the other way while the VMM runs.
		if jailedAtLaunch {
			r.p.jail = nil
		} else {
			r.p.jail = cfg
		}

		if err := r.p.Stop(r.ctx, ref); err != nil {
			t.Fatalf("jailed at launch %v: Stop = %v", jailedAtLaunch, err)
		}

		calls := r.l.history[len(r.l.history)-1].Calls()
		got := body(calls, "PUT /snapshot/create", "snapshot_path")

		want := fc.View{}.Path(filepath.Join(dir, fc.StateName+".new"))
		if launched.Jail != nil {
			want = fc.View{Root: fc.JailRoot(dir, launched.Binary)}.Path(filepath.Join(dir, fc.StateName+".new"))
		}

		if got != want {
			t.Fatalf("jailed at launch %v: snapshot_path = %q, want %q", jailedAtLaunch, got, want)
		}

		vm := r.vm(t, ref)
		if !vm.SnapshotValid || vm.SnapshotJailed != (launched.Jail != nil) {
			t.Fatalf("jailed at launch %v: valid %v, snapshot_jailed %v", jailedAtLaunch, vm.SnapshotValid, vm.SnapshotJailed)
		}

		if b, err := os.ReadFile(filepath.Join(dir, fc.StateName)); err != nil || len(b) == 0 {
			t.Fatalf("jailed at launch %v: the snapshot is not the VM's: %v", jailedAtLaunch, err)
		}
	}
}

// A commit of a running VMM takes the same view: its own launch's, not the process's.
func TestACommitUsesTheJailModeTheVMMWasLaunchedWith(t *testing.T) {
	r := newRig(t)
	r.p.jailer = func(context.Context) (string, error) { return "/pinned/jailer", nil }

	ref := r.create(t, "jc", redis)
	dir := r.p.dir(ref)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	r.p.jail = &fc.JailConfig{UIDBase: fc.DefaultJailUIDBase}

	const img = "sbx-snap-jc-cache:latest"
	if err := r.p.Commit(r.ctx, ref, img); err != nil {
		t.Fatal(err)
	}

	got := body(r.l.server(dir).Calls(), "PUT /snapshot/create", "snapshot_path")
	if want := filepath.Join(r.p.snapshotDir(img), fc.StateName); !strings.HasPrefix(got, filepath.Dir(want)) {
		t.Fatalf("an unjailed VMM was told %q, want a host path under %s", got, filepath.Dir(want))
	}
}

// Every drive the VMM is told to open read-only is staged read-only, so the jail never hands it
// to the VMM's uid (fc.Stage.ReadOnly), and every drive it writes is staged as the VM's own. The
// expectation is what the VMM was actually told - each PUT /drives call's path and is_read_only.
func TestEveryReadOnlyDriveIsStagedReadOnly(t *testing.T) {
	r := jailRig(t)
	r.g.available = false
	r.create(t, "ro", redis)

	staged := map[string]bool{}
	for _, f := range r.l.specs[0].Jail.Files {
		staged[f.Name] = f.ReadOnly
	}

	drives := 0

	for _, c := range r.l.history[0].Calls() {
		if c.Method != "PUT" || !strings.HasPrefix(c.Path, "/drives/") {
			continue
		}

		drives++
		name := strings.TrimPrefix(c.Body["path_on_host"].(string), "/")
		ro, _ := c.Body["is_read_only"].(bool)

		if got, ok := staged[name]; !ok || got != ro {
			t.Fatalf("%s: the VMM opens it read-only=%v, staged read-only=%v (staged at all: %v)", name, ro, got, ok)
		}
	}

	if drives == 0 {
		t.Fatal("no drive was attached; the test proves nothing")
	}

	// A volume carries its own mode from the record into the stage.
	vm := &fcVM{Ref: "sbx-x-y", Volumes: []fcVolume{{Name: "ro", ReadOnly: true}, {Name: "rw"}}}
	for _, s := range r.p.driveStages(vm) {
		for i, v := range vm.Volumes {
			if s.Name == volumeStage(i) && s.ReadOnly != v.ReadOnly {
				t.Fatalf("volume %s (read-only %v) staged read-only=%v", v.Name, v.ReadOnly, s.ReadOnly)
			}
		}
	}
}

// A paused VM resumed - by a Start, or by the claim of a frozen warm-pool member - goes through the
// same host-guard recheck a wake does, and is refused, still paused, when the guard cannot be put
// back: resuming it would hand its guests a host that is open to them.
func TestAResumeIsRefusedWhenTheHostGuardCannotBeRechecked(t *testing.T) {
	gone := errors.New("refusing to start a microVM on sbxfc1: its host guard was missing")

	resumes := map[string]func(r *rig) (string, error){
		"start": func(r *rig) (string, error) {
			ref := r.create(t, "rs", redis)
			if err := r.p.Start(r.ctx, ref); err != nil {
				t.Fatal(err)
			}

			if err := r.p.client(ref).Pause(r.ctx); err != nil {
				t.Fatal(err)
			}

			return ref, r.p.Start(r.ctx, ref)
		},
		"claim of a frozen member": func(r *rig) (string, error) {
			ref := r.create(t, "rc", apiSvc("member-token"))
			if err := r.p.Park(r.ctx, ref, true); err != nil {
				t.Fatal(err)
			}

			return ref, r.p.Claim(r.ctx, ref, "caller-token", nil)
		},
	}

	for name, resume := range resumes {
		r, _ := osbRig(t)
		r.n.guardErr = gone

		ref, err := resume(r)
		if !errors.Is(err, gone) {
			t.Fatalf("%s with the guard unrecoverable = %v, want the guard's refusal", name, err)
		}

		if s := r.l.server(r.p.dir(ref)); s != nil && s.State() == fc.StateRunning {
			t.Fatalf("%s: the VM was resumed on a bridge whose guard could not be put back", name)
		}

		if slot := r.vm(t, ref).Slot; !slices.Contains(r.n.rechecked, slot) {
			t.Fatalf("%s: rechecked slots %v, not the VM's %d", name, r.n.rechecked, slot)
		}
	}
}
