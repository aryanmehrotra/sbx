package provider

// The warm pool on a microVM (PoolParker): a member is an ordinary API VM, born running, then
// parked - asleep by default, frozen when asked - and claimed by one wake plus one re-key under
// the VM's lock. See "Warm-pool members wait asleep on a microVM" in DECISIONS.md.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

// Park puts a running API VM into a member's wait. Asleep is the sleep every idle VM takes: seal,
// a Full snapshot (it was born running, so nothing was restored to diff against), the VMM ended.
// Frozen is Firecracker's pause, with nothing sealed: the VM never leaves memory, so nothing of it
// is on disk for anyone to restore.
func (p *fcProvider) Park(ctx context.Context, ref string, frozen bool) error {
	unlock := p.lock(ref)
	defer unlock()

	vm, err := p.load(ref)
	if err != nil {
		return err
	}

	if vm.OSB == "" {
		return fmt.Errorf("%s is not an OpenSandbox API sandbox; only those are warm-pool members", ref)
	}

	state, err := p.running(ctx, ref)
	if err != nil {
		return err
	}

	if state != fc.StateRunning {
		return fmt.Errorf("%s is %s, not running: a member is parked once, straight after it is made",
			ref, stateName(state))
	}

	vm.Pooled = true
	if err := p.save(vm); err != nil {
		return err
	}

	if frozen {
		return p.client(ref).Pause(ctx)
	}

	return p.sleep(ctx, vm)
}

// Claim wakes a parked member and gives it the caller's identity with one re-key. The record
// takes the caller's token and env first, so the restore's own re-key is that one, and so every
// later wake re-keys with them.
func (p *fcProvider) Claim(ctx context.Context, ref, token string, env map[string]string) error {
	if token == "" {
		return errors.New("a claim needs the token the API minted for its caller")
	}

	if !p.guest.Available() {
		return errors.New("a warm-pool claim re-keys the member's execd over vsock, and this build has no guest channel")
	}

	unlock := p.lock(ref)
	defer unlock()

	vm, err := p.load(ref)
	if err != nil {
		return err
	}

	if !vm.Pooled {
		return fmt.Errorf("%s is not a parked warm-pool member", ref)
	}

	state, err := p.running(ctx, ref)
	if err != nil {
		return err
	}

	if vm.BootToken == "" {
		vm.BootToken = vm.AccessToken
	}

	vm.AccessToken, vm.ClaimEnv = token, maps.Clone(env)

	switch state {
	case "":
		return p.claimAsleep(ctx, vm)
	case fc.StatePaused, fc.StateRunning:
		return p.claimAwake(ctx, vm, state)
	default:
		return fmt.Errorf("%s is %s, which no claim can wake", ref, stateName(state))
	}
}

// claimAsleep restores a member's snapshot. restore re-keys before it returns and stops the VM if
// that fails, so a member that could not take the caller's identity is never left answering.
func (p *fcProvider) claimAsleep(ctx context.Context, vm *fcVM) error {
	if !vm.SnapshotValid {
		// Parking kills the VMM whenever its snapshot fails, so this is a member that died awake:
		// its memory is gone and it is no warmer than a cold create.
		return fmt.Errorf("%s has no valid snapshot to restore; it is no longer warm", vm.Ref)
	}

	// Parked with the jailer the other way: its snapshot names drive paths this VMM cannot open
	// (jail-root paths, or host paths a chroot hides). Start cold-boots such a VM; a member that
	// would need a cold boot is no warmer than a cold create, so it is refused, never loaded.
	if vm.SnapshotJailed != (p.jail != nil) {
		return fmt.Errorf("%s was parked with the jailer %s and it is %s now: its snapshot names drive "+
			"paths this VMM cannot open, so it is no longer warm", vm.Ref, onOff(vm.SnapshotJailed), onOff(p.jail != nil))
	}

	release, err := p.bootSlot(ctx)
	if err != nil {
		return err
	}
	defer release()

	// A stale VMM that is up but not started would hold the API socket the restore binds.
	_ = p.launch.Kill(ctx, p.dir(vm.Ref))

	if err := p.restore(ctx, vm); err != nil {
		return err
	}

	vm.Pooled = false

	if err := p.save(vm); err != nil {
		return errors.Join(fmt.Errorf("recording the claim of %s: %w", vm.Ref, err), p.abandon(ctx, vm))
	}

	return nil
}

// claimAwake resumes a frozen member (or takes one a stray wake already resumed) and re-keys it
// at a newer generation. A failure abandons the VM: stopped, and cold-booted if ever woken.
func (p *fcProvider) claimAwake(ctx context.Context, vm *fcVM, state string) error {
	if state == fc.StatePaused {
		if err := p.client(vm.Ref).Resume(ctx); err != nil {
			return errors.Join(fmt.Errorf("resuming %s: %w", vm.Ref, err), p.abandon(ctx, vm))
		}
	}

	vm.Generation++
	vm.Pooled = false

	if err := p.rekey(ctx, vm); err != nil {
		return errors.Join(fmt.Errorf("re-keying %s for its caller: %w - it was stopped rather than "+
			"handed out with the member's identity", vm.Ref, err), p.abandon(ctx, vm))
	}

	if err := p.save(vm); err != nil {
		return errors.Join(fmt.Errorf("recording the claim of %s: %w", vm.Ref, err), p.abandon(ctx, vm))
	}

	return nil
}

// reassertClaim re-keys a claimed VM that has just cold-booted. Its agent drive was built for the
// member, so the execd it boots holds the member's token and none of the caller's env; until this
// re-key it answers only that token, which only the API's process ever held. The caller holds the
// lock; a failure stops the VM.
func (p *fcProvider) reassertClaim(ctx context.Context, vm *fcVM) error {
	if vm.BootToken == "" || !p.guest.Available() {
		return nil
	}

	bctx, cancel := context.WithTimeout(ctx, p.bootTimeout)
	defer cancel()

	if err := p.ready(bctx, vm, p.dir(vm.Ref)); err != nil {
		return errors.Join(fmt.Errorf("%s cold-booted but never served, so its caller's identity could not "+
			"be given back to it: %w", vm.Ref, err), p.abandon(ctx, vm))
	}

	vm.Generation++

	if err := p.rekey(bctx, vm); err != nil {
		return errors.Join(fmt.Errorf("re-keying %s with its caller's identity after a cold boot: %w - "+
			"stopped rather than left answering the warm-pool member's token", vm.Ref, err), p.abandon(ctx, vm))
	}

	return nil
}

// claimEnv is a claim's env as a re-key carries it, sorted; nil when the claim gave none, which
// leaves execd's env as it is.
func claimEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}

	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}

	return out
}

func stateName(s string) string {
	if s == "" {
		return "asleep"
	}

	return s
}
