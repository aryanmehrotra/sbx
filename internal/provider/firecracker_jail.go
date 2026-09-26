package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

// view is how vm's VMM sees the host's files: the host's own paths unjailed, its jail's root
// otherwise (fc.View).
func (p *fcProvider) view(vm *fcVM) fc.View {
	if p.jail == nil {
		return fc.View{}
	}

	return fc.View{Root: fc.JailRoot(p.dir(vm.Ref), vm.Binary)}
}

// launchSpec is the launch of vm's VMM with stage put in its jail - or, unjailed, the plain
// launch it always was.
func (p *fcProvider) launchSpec(ctx context.Context, vm *fcVM, stage []fc.Stage) (fc.LaunchSpec, error) {
	s := fc.LaunchSpec{Binary: vm.Binary, Dir: p.dir(vm.Ref), ID: vm.Instance}
	if p.jail == nil {
		return s, nil
	}

	jailer, err := p.jailer(ctx)
	if err != nil {
		return s, fmt.Errorf("the firecracker jailer: %w", err)
	}

	uid := p.jail.UID(vm.addr())
	s.Jail = &fc.JailSpec{Jailer: jailer, UID: uid, GID: uid, CPUs: vm.VCPU, MemMiB: vm.MemMiB, Files: stage}

	return s, nil
}

// driveStages are vm's drives, by the names its VMM opens them at: each is the VM's own file,
// hard-linked into the jail and given to its uid.
func (p *fcProvider) driveStages(vm *fcVM) []fc.Stage {
	dir := p.dir(vm.Ref)
	out := []fc.Stage{
		{Name: "agent.ext4", Host: filepath.Join(dir, "agent.ext4")},
		{Name: fc.RootfsName, Host: filepath.Join(dir, fc.RootfsName)},
	}

	for i, v := range vm.Volumes {
		out = append(out, fc.Stage{Name: volumeStage(i), Host: p.volumePath(v.Name)})
	}

	return out
}

// volumeStage is the i-th volume's name in a jail: by position, so a volume called "rootfs"
// cannot land on the root filesystem's name.
func volumeStage(i int) string { return volumeDriveID(i) + ".ext4" }

func onOff(b bool) string {
	if b {
		return "on"
	}

	return "off"
}

// guardedNetwork is the host network with, managed, the host closed to each bridge's guests
// except for the egress filter's port (fc.Guard), failing closed; unmanaged, no rule of sbx's at
// all. Jailed, each VM's tap is made for its VMM's uid.
func guardedNetwork(firewall fc.FirewallMode, jail *fc.JailConfig) *fc.IPNetwork {
	n := fc.NewIPNetwork(os.Getuid())
	n.Warn = func(msg string) { fmt.Fprintln(os.Stderr, "  warning: "+msg) }

	if firewall != fc.FirewallUnmanaged {
		n.Guard = fc.NewGuard(EgressProxyPort)
	}

	if jail != nil {
		n.OwnerOf = jail.UID
		n.TapOwner = fc.SysTapOwner
	}

	return n
}
