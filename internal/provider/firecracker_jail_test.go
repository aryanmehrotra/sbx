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
