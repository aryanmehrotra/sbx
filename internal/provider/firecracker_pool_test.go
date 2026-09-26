package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

var _ PoolParker = (*fcProvider)(nil)

// lastRekey is the most recent re-key the guest was sent, and the VM state it arrived in.
func (r *rig) lastRekey(t *testing.T) (fc.Rekey, string) {
	t.Helper()

	r.g.mu.Lock()
	defer r.g.mu.Unlock()

	if len(r.g.rekeys) == 0 {
		t.Fatal("no re-key was sent")
	}

	n := len(r.g.rekeys) - 1

	return r.g.rekeys[n], r.g.rekeyState[n]
}

func (r *rig) rekeyCount() int {
	r.g.mu.Lock()
	defer r.g.mu.Unlock()

	return len(r.g.rekeys)
}

// An asleep member is a Full snapshot with no VMM; its claim restores it and re-keys it once, with
// the caller's token, the caller's env and a new control secret - and the record carries that
// token from then on, so the next ordinary wake re-keys with it too.
func TestAnAsleepMemberIsClaimedWithTheCallersIdentity(t *testing.T) {
	r, _ := osbRig(t)

	ref := r.create(t, "osb-pool1", apiSvc("member-token"))
	dir := r.p.dir(ref)

	if err := r.p.Park(r.ctx, ref, false); err != nil {
		t.Fatal(err)
	}

	vm := r.vm(t, ref)
	if s := r.l.server(dir); s != nil {
		t.Fatalf("a parked asleep member still has a VMM (%s)", s.State())
	}

	if !vm.Pooled || !vm.SnapshotValid {
		t.Fatalf("parked record = pooled %v, snapshot valid %v", vm.Pooled, vm.SnapshotValid)
	}

	if _, err := os.Stat(filepath.Join(dir, fc.MemName)); err != nil {
		t.Fatalf("no memory file after parking asleep: %v", err)
	}

	if len(r.g.seals) != 1 || r.g.seals[0] != fc.StateRunning {
		t.Fatalf("seals = %v, want one, of the running VM", r.g.seals)
	}

	before := vm.secret()

	if err := r.p.Claim(r.ctx, ref, "caller-token", map[string]string{"B": "2"}); err != nil {
		t.Fatal(err)
	}

	k, state := r.lastRekey(t)

	switch {
	case k.AccessToken != "caller-token":
		t.Fatalf("re-key token %q, want the caller's", k.AccessToken)
	case !slices.Equal(k.Env, []string{"B=2"}):
		t.Fatalf("re-key env %q, want the caller's", k.Env)
	case k.ControlSecret == before || k.ControlSecret == "":
		t.Fatal("the claim kept the parked member's control secret")
	case state != fc.StateRunning:
		t.Fatalf("re-keyed in state %q, want a restored, running VM", state)
	}

	if s := r.l.server(dir); s == nil || s.State() != fc.StateRunning {
		t.Fatal("a claimed member is not running")
	}

	vm = r.vm(t, ref)
	if vm.AccessToken != "caller-token" || vm.Pooled || vm.BootToken != "member-token" {
		t.Fatalf("claimed record: token %q pooled %v boot token %q", vm.AccessToken, vm.Pooled, vm.BootToken)
	}

	// Asleep and awake as an ordinary sandbox: the member's token never comes back.
	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if k, _ := r.lastRekey(t); k.AccessToken != "caller-token" {
		t.Fatalf("a wake after the claim re-keyed with %q", k.AccessToken)
	}
}

// A frozen member waits paused with its memory resident, never sealed; its claim resumes it and
// re-keys it at a newer generation.
func TestAFrozenMemberIsResumedAndReKeyed(t *testing.T) {
	r, _ := osbRig(t)

	ref := r.create(t, "osb-pool2", apiSvc("member-token"))
	dir := r.p.dir(ref)

	if err := r.p.Park(r.ctx, ref, true); err != nil {
		t.Fatal(err)
	}

	if s := r.l.server(dir); s == nil || s.State() != fc.StatePaused {
		t.Fatal("a frozen member is not paused")
	}

	if len(r.g.seals) != 0 {
		t.Fatalf("a frozen member was sealed: %v", r.g.seals)
	}

	gen := r.vm(t, ref).Generation

	if err := r.p.Claim(r.ctx, ref, "caller-token", nil); err != nil {
		t.Fatal(err)
	}

	k, state := r.lastRekey(t)
	if k.AccessToken != "caller-token" || state != fc.StateRunning || k.Generation <= gen || k.Env != nil {
		t.Fatalf("re-key %+v in state %q, want the caller's token on a resumed VM at a generation past %d",
			k, state, gen)
	}

	if s := r.l.server(dir); s == nil || s.State() != fc.StateRunning {
		t.Fatal("a claimed frozen member is not running")
	}
}

// A member whose re-key fails is never handed out: the claim errors and the VM is stopped, so
// nothing is left answering with the member's identity.
func TestAFailedClaimReKeyStopsTheMember(t *testing.T) {
	for _, frozen := range []bool{false, true} {
		r, _ := osbRig(t)

		ref := r.create(t, "osb-pool3", apiSvc("member-token"))

		if err := r.p.Park(r.ctx, ref, frozen); err != nil {
			t.Fatal(err)
		}

		r.g.failRekey = errors.New("execd refused")

		if err := r.p.Claim(r.ctx, ref, "caller-token", nil); err == nil {
			t.Fatalf("frozen=%v: a claim whose re-key failed succeeded", frozen)
		}

		if s := r.l.server(r.p.dir(ref)); s != nil {
			t.Fatalf("frozen=%v: the member is still up (%s) after its re-key failed", frozen, s.State())
		}
	}
}

// A claimed VM that dies awake cold-boots from an agent drive that still carries the member's
// token. It is re-keyed with the caller's before Start returns, env included.
func TestAColdBootAfterAClaimReassertsTheCallersIdentity(t *testing.T) {
	r, _ := osbRig(t)

	ref := r.create(t, "osb-pool4", apiSvc("member-token"))

	if err := r.p.Park(r.ctx, ref, false); err != nil {
		t.Fatal(err)
	}

	if err := r.p.Claim(r.ctx, ref, "caller-token", map[string]string{"B": "2"}); err != nil {
		t.Fatal(err)
	}

	// Dies awake: the VMM goes, the snapshot is invalid.
	_ = r.l.Kill(r.ctx, r.p.dir(ref))

	n := r.rekeyCount()

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if r.rekeyCount() != n+1 {
		t.Fatalf("a cold boot after a claim sent %d re-keys, want 1", r.rekeyCount()-n)
	}

	k, _ := r.lastRekey(t)
	if k.AccessToken != "caller-token" || !slices.Equal(k.Env, []string{"B=2"}) || k.Secret != r.vm(t, ref).ControlSecret {
		t.Fatalf("re-key after the cold boot = %+v, want the caller's identity under the boot secret", k)
	}

	// An unclaimed VM's cold boot needs none: its agent drive is its identity.
	r2, _ := osbRig(t)
	ref2 := r2.create(t, "osb-plain", apiSvc("tok"))
	_ = r2.l.Kill(r2.ctx, r2.p.dir(ref2))

	if err := r2.p.Start(r2.ctx, ref2); err != nil {
		t.Fatal(err)
	}

	if r2.rekeyCount() != 0 {
		t.Fatalf("an unclaimed cold boot sent %d re-keys", r2.rekeyCount())
	}
}

// Only a parked API VM can be claimed, and only a running API VM parked.
func TestParkAndClaimRefuseWhatIsNotAMember(t *testing.T) {
	r, _ := osbRig(t)

	ref := r.create(t, "osb-pool5", apiSvc("member-token"))

	if err := r.p.Claim(r.ctx, ref, "caller-token", nil); err == nil {
		t.Fatal("a VM that was never parked was claimed")
	}

	if err := r.p.Park(r.ctx, ref, false); err != nil {
		t.Fatal(err)
	}

	if err := r.p.Park(r.ctx, ref, false); err == nil {
		t.Fatal("an asleep VM was parked again")
	}

	if err := r.p.Claim(r.ctx, ref, "", nil); err == nil {
		t.Fatal("a claim with no token succeeded")
	}

	spec := r.create(t, "plain", redis) // a sandbox.json service: not the API's

	if err := r.p.Park(r.ctx, spec, false); err == nil {
		t.Fatal("a sandbox.json VM was parked")
	}
}

// A disk snapshot of a claimed VM keeps none of its claim: not the boot token, not the env.
func TestForgetSecretsDropsTheClaim(t *testing.T) {
	vm := forgetSecrets(fcVM{AccessToken: "a", BootToken: "b", ClaimEnv: map[string]string{"K": "v"}, Pooled: true})
	if vm.BootToken != "" || vm.ClaimEnv != nil || vm.Pooled || vm.AccessToken != "" {
		t.Fatalf("forgetSecrets kept %+v", vm)
	}
}

// An asleep member's claim is a restore, and a restore takes a boot slot like any wake: a burst
// of claims cannot restore more VMs at once than the host has CPUs for.
func TestAnAsleepClaimWaitsForABootSlot(t *testing.T) {
	r, _ := osbRig(t)

	ref := r.create(t, "osb-pool6", apiSvc("member-token"))

	if err := r.p.Park(r.ctx, ref, false); err != nil {
		t.Fatal(err)
	}

	r.p.boots = make(chan struct{}, 1)
	r.p.boots <- struct{}{} // every slot taken

	ctx, cancel := context.WithTimeout(r.ctx, 50*time.Millisecond)
	defer cancel()

	if err := r.p.Claim(ctx, ref, "caller-token", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("claim with no boot slot free = %v, want it to wait for one", err)
	}

	<-r.p.boots

	if err := r.p.Claim(r.ctx, ref, "caller-token", nil); err != nil {
		t.Fatalf("claim once a slot is free: %v", err)
	}
}
