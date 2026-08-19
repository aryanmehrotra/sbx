package cli

import (
	"context"
	"fmt"

	"github.com/aryanmehrotra/sbx/internal/provider"
)

// Memory checkpoint and resume.
//
// Snapshot/fork save a filesystem; this saves the running process. `sbx checkpoint` CRIU-dumps
// every running service's memory and freezes it; `sbx resume` starts them again from that dump,
// so an agent's half-finished REPL, a warmed cache or a connection mid-handshake comes back
// exactly as it was - the one thing E2B and zeropod do that a disk snapshot cannot.
//
// It is honest about where it cannot run. `docker checkpoint` is CRIU behind the daemon's
// experimental flag, which Docker Desktop and Colima on macOS leave off, so on a Mac this
// refuses with a reason and points at snapshot/fork. On a cluster the mechanism is a per-node
// shim (that is what zeropod is), the operator's to install, so the kubernetes provider does
// not offer it. `sbx doctor` reports `docker checkpoint` so you know before you rely on it.
//
// This is the EXPLICIT pair, driven by you, the way snapshot/fork are - not yet a
// memory-preserving idle sleep, which would checkpoint on the daemon's idle timer and restore
// on the next connection. That integration is a separate step; freezing and resuming on demand
// is the capability this adds.

// Checkpoint dumps every running service of a sandbox under a name and freezes it.
func Checkpoint(ctx context.Context, p provider.Provider, sandbox, name string) error {
	if err := ValidateName("sandbox", sandbox); err != nil {
		return err
	}

	if err := ValidateSnapshotName(name); err != nil {
		return err
	}

	if name == "" {
		return fmt.Errorf("a checkpoint needs a name: sbx checkpoint <sandbox> <name>")
	}

	// Asked once, at the top, so a backend without a path to this says so before anything is
	// half done - the same negotiation snapshot and --isolation use.
	cp, err := provider.CheckpointerFor(p)
	if err != nil {
		return err
	}

	units, err := p.List(ctx, sandbox)
	if err != nil {
		return err
	}

	if len(units) == 0 {
		return UnknownSandbox(ctx, p, sandbox)
	}

	saved := 0

	for _, u := range units {
		if !u.Running {
			continue // nothing to dump; a resume brings the frozen ones back
		}

		if err := cp.Checkpoint(ctx, u.Ref, name, false); err != nil {
			return fmt.Errorf("checkpointing %s: %w", u.Service, err)
		}

		fmt.Printf("  %-24s checkpointed and frozen\n", u.Service)
		saved++
	}

	if saved == 0 {
		fmt.Printf("sandbox %q has nothing awake to checkpoint\n", sandbox)
		return nil
	}

	fmt.Printf("sandbox %q checkpointed as %q - %d service(s) frozen with memory intact\n", sandbox, name, saved)
	fmt.Printf("  resume with: sbx resume %s %s\n", sandbox, name)

	return nil
}

// Resume restores every service of a sandbox from a named checkpoint.
func Resume(ctx context.Context, p provider.Provider, sandbox, name string) error {
	if err := ValidateName("sandbox", sandbox); err != nil {
		return err
	}

	if err := ValidateSnapshotName(name); err != nil {
		return err
	}

	if name == "" {
		return fmt.Errorf("a resume needs the checkpoint name: sbx resume <sandbox> <name>")
	}

	cp, err := provider.CheckpointerFor(p)
	if err != nil {
		return err
	}

	units, err := p.List(ctx, sandbox)
	if err != nil {
		return err
	}

	if len(units) == 0 {
		return UnknownSandbox(ctx, p, sandbox)
	}

	resumed := 0

	for _, u := range units {
		if err := cp.Restore(ctx, u.Ref, name); err != nil {
			return fmt.Errorf("resuming %s: %w", u.Service, err)
		}

		fmt.Printf("  %-24s resumed from %q\n", u.Service, name)
		resumed++
	}

	fmt.Printf("sandbox %q resumed - %d service(s) back with memory and processes intact\n", sandbox, resumed)

	return nil
}
