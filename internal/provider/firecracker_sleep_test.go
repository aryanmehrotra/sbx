package provider

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aryanmehrotra/sbx/internal/fc"
)

// Known issue of v0.11 (release notes): under host memory pressure a sleep's Seal answer can be
// late or lost. Seal is idempotent in execd (rekey.go: it sets sealed and clears the replay
// record, nothing else), so a Seal whose answer was lost is asked again, and a 204 on the retry
// proves the guest is sealed. The sleep then snapshots, as if the first answer had arrived.
func TestALostSealAnswerIsAskedAgainAndTheSleepSnapshots(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "k3a", redis)
	dir := r.p.dir(ref)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	seals := len(r.g.seals)
	r.g.sealLost = 1

	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatalf("Stop = %v: a Seal whose first answer was lost was not asked again", err)
	}

	if got := len(r.g.seals) - seals; got != 2 {
		t.Fatalf("Seal reached execd %d times, want 2 (the lost answer, then the confirmation)", got)
	}

	vm := r.vm(t, ref)
	if !vm.SnapshotValid {
		t.Fatalf("the sleep did not snapshot after the seal was confirmed: %+v", vm)
	}

	if r.l.server(dir) != nil {
		t.Fatal("the VMM is still up after a sleep that snapshotted")
	}
}

// A Seal that never answers is not proof the guest is unsealed - it may have applied it. The VM
// is neither snapshotted (identity must not leak into a restore) nor stopped (its memory is what
// the sleep was meant to keep): it is re-keyed, which proves execd unsealed and serving, and the
// sleep reports that it failed with the VM still up, so the daemon's next tick can try again.
func TestASealThatNeverConfirmsKeepsTheVMRunningAndReKeyed(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "k3b", redis)
	dir := r.p.dir(ref)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	before := r.vm(t, ref)
	rekeys := len(r.g.rekeys)
	r.g.failSeal = errors.New("context deadline exceeded")

	err := r.p.Stop(r.ctx, ref)
	if err == nil {
		t.Fatal("Stop succeeded without a confirmed seal")
	}

	if !errors.Is(err, ErrStillRunning) {
		t.Fatalf("Stop = %v, want it to say the VM is still running", err)
	}

	if s := r.l.server(dir); s == nil || s.State() != fc.StateRunning {
		t.Fatal("a VM whose seal was never confirmed was stopped, losing its memory")
	}

	if got := len(r.g.rekeys) - rekeys; got != 1 {
		t.Fatalf("re-keys after the unconfirmed seal = %d, want 1: nothing proved execd unsealed", got)
	}

	vm := r.vm(t, ref)
	if vm.SnapshotValid {
		t.Fatal("a snapshot was marked valid for a VM that was never sealed")
	}

	if vm.LiveSecret == before.LiveSecret || vm.LiveSecret != r.g.rekeys[len(r.g.rekeys)-1].ControlSecret {
		t.Fatalf("the record does not hold the secret the confirming re-key installed: %+v", vm)
	}

	if vm.Generation <= before.Generation {
		t.Fatalf("generation %d after the re-key, want > %d: execd refuses a re-key that is not newer",
			vm.Generation, before.Generation)
	}

	if vm.SealStrikes != 1 {
		t.Fatalf("SealStrikes = %d, want 1", vm.SealStrikes)
	}
}

// A guest may not veto its sleep for ever: the VM keeps its memory through maxSealStrikes
// failed sleeps in a row, and the next one stops it as v0.11 did (its next wake cold-boots).
// A sleep that snapshots resets the count.
func TestAGuestThatRefusesEverySealIsStoppedAfterTheStrikes(t *testing.T) {
	r := newRig(t)
	ref := r.create(t, "k3c", redis)
	dir := r.p.dir(ref)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	r.g.failSeal = errors.New("execd busy")

	for i := 1; i < maxSealStrikes; i++ {
		if err := r.p.Stop(r.ctx, ref); !errors.Is(err, ErrStillRunning) {
			t.Fatalf("failed sleep %d = %v, want the VM kept running", i, err)
		}
	}

	err := r.p.Stop(r.ctx, ref)
	if err == nil || errors.Is(err, ErrStillRunning) || !strings.Contains(err.Error(), "cold boot") {
		t.Fatalf("failed sleep %d = %v, want the VM stopped and its next wake a cold boot", maxSealStrikes, err)
	}

	if r.l.server(dir) != nil {
		t.Fatalf("a guest that refused %d sleeps in a row is still holding its memory", maxSealStrikes)
	}

	if vm := r.vm(t, ref); vm.SealStrikes != 0 || vm.SnapshotValid {
		t.Fatalf("record after the forced stop = %+v", vm)
	}

	// Once it sleeps properly, the count starts again.
	r.g.failSeal = nil

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	r.g.failSeal = errors.New("execd busy")

	if err := r.p.Stop(r.ctx, ref); !errors.Is(err, ErrStillRunning) {
		t.Fatal(err)
	}

	r.g.failSeal = nil

	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	if vm := r.vm(t, ref); vm.SealStrikes != 0 || !vm.SnapshotValid {
		t.Fatalf("a sleep that snapshotted left %+v", vm)
	}
}

// Each Seal attempt is given longer than the last, and a guest that stalls the first and answers
// the second sleeps with its memory kept.
func TestSealAttemptsAreGivenLongerEachTime(t *testing.T) {
	r := newRig(t)
	r.p.sealBudgets = sealBudgetsForTest

	ref := r.create(t, "k3d", redis)

	if err := r.p.Start(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	r.g.sealBudgets = nil
	r.g.sealStall = 1

	if err := r.p.Stop(r.ctx, ref); err != nil {
		t.Fatal(err)
	}

	got := r.g.sealBudgets
	if len(got) != 2 {
		t.Fatalf("Seal attempts = %d (%v), want 2", len(got), got)
	}

	if got[0] > sealBudgetsForTest[0] || got[1] <= sealBudgetsForTest[0] || got[1] > sealBudgetsForTest[1] {
		t.Fatalf("attempt budgets = %v, want within %v, each longer than the last", got, sealBudgetsForTest)
	}

	if !r.vm(t, ref).SnapshotValid {
		t.Fatal("no snapshot after the second attempt answered")
	}
}

// The default schedule is what the release notes promise, and its total bounds how long a sleep
// holds the VM's lock (a wake waits behind it).
func TestTheDefaultSealScheduleEscalatesAndIsBounded(t *testing.T) {
	if len(defaultSealBudgets) < 2 {
		t.Fatalf("defaultSealBudgets = %v: one attempt is v0.11's behaviour", defaultSealBudgets)
	}

	var total int64

	for i, b := range defaultSealBudgets {
		if i > 0 && b <= defaultSealBudgets[i-1] {
			t.Fatalf("defaultSealBudgets = %v: each attempt must be given longer than the last", defaultSealBudgets)
		}

		total += int64(b)
	}

	if total > int64(maxSealWait) {
		t.Fatalf("defaultSealBudgets = %v sum past %s", defaultSealBudgets, maxSealWait)
	}
}

// sealBudgetsForTest is every rig's Seal schedule: the default's shape, in milliseconds.
var sealBudgetsForTest = []time.Duration{40 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond}
