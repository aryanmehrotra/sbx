package provider

import "context"

// PoolParker is a RunsAgent provider's warm-pool support: a sandbox the OpenSandbox API made
// ahead of demand is parked until a create claims it, and the claim gives it the caller's
// identity before anyone can reach it.
//
// Docker's pool needs no provider support - a member waits running (or `docker pause`d) and its
// claim is execd's own POST /sbx/claim. A microVM member can instead wait asleep, a memory
// snapshot on disk holding no RAM, and a VM restored from a snapshot must be re-keyed by its host
// over vsock before it answers anyone (DECISIONS.md, "Warm-pool members wait asleep on a microVM").
type PoolParker interface {
	// Park puts a running API sandbox into a member's wait: asleep (execd sealed, a Full
	// snapshot, the VMM ended) or, with frozen, paused with its memory resident. A failure
	// leaves the VM stopped or as it was; the member is not to be used.
	Park(ctx context.Context, ref string, frozen bool) error

	// Claim wakes a parked member - restored or resumed - and re-keys its agent with token (the
	// API's, minted for this create), env (the caller's) and a fresh control secret, before it
	// returns. The token is the sandbox's from then on, across every later sleep, wake and cold
	// boot. A failed re-key stops the VM and is an error: a member that could not be given the
	// caller's identity is never handed out.
	Claim(ctx context.Context, ref, token string, env map[string]string) error
}
