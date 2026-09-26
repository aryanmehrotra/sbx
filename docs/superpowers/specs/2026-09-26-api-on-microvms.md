# The OpenSandbox API and warm pool on Firecracker — plan

> **Why:** anonymous or untrusted code (a public "free box", a hosted leaderboard endpoint) must not
> share a kernel with the host. v0.11.0 ships microVMs for `sandbox.json`, but the OpenSandbox API
> answers 501 on them. This release closes that: **every API route, and the warm pool, on
> `--provider firecracker`, proven by upstream's own conformance suite in CI on real KVM.**

## Goal / done

`sbx serve --provider firecracker --osb-addr … [--osb-pool IMAGE=N]` passes the v0.10.0
conformance tier (and the use-case suite) on Linux with `/dev/kvm`, in the CI `microvm` job on
GitHub's x86_64 runner. The docker path is unchanged and still passes the same gates.

## Shape

| concern | decision |
|---|---|
| execd placement | new optional capability **`RunsAgent`** (named for the want: "this provider already runs execd as the sandbox's agent"). OSB skips the execd volume seed, the `/opt/sbx` mount and the entrypoint rewrite when the provider has it. `Injector` stays docker's. |
| execd token | **OSB owns it.** The provider boots/re-keys execd with the token in the spec env; it never mints its own for an API sandbox. |
| endpoint 44772 | existing `provider.GuestDialer` (vsock). Other ports: the existing `/proxy/{port}` through execd. |
| readiness / health | execd `/ping` over vsock; health already runs through execd on create + cold boot. |
| pause / resume | `Pauser` on firecracker = VM pause/resume (memory kept). |
| snapshots / templates | native VM snapshot (memory + disk). Create-from-snapshot = restore **as a new sandbox**: needs per-VM drive paths → a VM restored from another's snapshot gets its drives copied/reflinked into its own dir and `network_overrides`/`vsock_override`; always re-keyed (identity never shared). If that proves unsafe, create-from-snapshot restores the disk only (cold boot) and says so. |
| volumes | `pvc` → an ext4 image file per claim, attached as an extra virtio block drive, namespaced like docker's; `host` refused by name (Firecracker has no virtio-fs). `ossfs` refused as today. |
| networkPolicy / egress | the existing egress filter, served by the daemon on each VM bridge's gateway address (Linux has the bridge natively); guests get it as their only route out. Live updates + CIDR reuse `EgressControl`. `defaultAction: allow` needs a NAT'd path through the filter only — refuse if it can't be enforced. |
| warm pool | members are ordinary API microVMs created, health-checked, snapshotted asleep (disk cost shown in doctor); claim = restore + re-key + env + identity. `--osb-pool-freeze` = keep members restored and paused (faster claim, costs RAM). Fork-from-one-template is a follow-up measured separately. |
| helper VM (Mac/Windows) | out of scope for this release: `--osb-addr` stays refused there with its current message. Linux direct first. *Amended (K5): served from the helper VM's daemon and fronted on the host; see DECISIONS.md, "The OpenSandbox API through a helper VM runs in the VM".* |

## Out of scope
Mac/Windows helper-VM API; snapshot fork for pool speed; host volumes; the public gateway service.

## Test plan
1. Unit: every OSB route against a fake RunsAgent provider; firecracker provider against fcfake.
2. CI `microvm` job: `scripts/osb-conformance.sh --tier v0.10.0 --provider firecracker` + the use-case suite with `--provider firecracker` (cases that need docker-only features skip by name).
3. Local: the same inside a colima helper VM (arm64, nested) as a smoke test.
4. Must not regress: the docker tier 50/0/2, 15/15 use cases, Linux `-race`, the existing microVM e2e.

## Risks accepted
Snapshot files cost disk per pool member (≈ VM RAM each); cold create stays seconds until fork lands.

## Amended 2026-09-26

What v0.12 ships differs from the plan above in three places; each is a decision, with its reason.

- **The warm pool on microVMs moves to v0.13.** `--osb-pool` with `--provider firecracker` is refused
  at startup with the reason. A pool member is a VM snapshotted asleep and restored per claim,
  which multiplies what a CI run has to hold (a snapshot per member, ≈ its RAM on disk, and a
  restore + re-key per claim) on the one runner class that has `/dev/kvm`. It lands once the
  single-VM paths it is built from have a green CI record of their own, proved there first rather
  than on a laptop that cannot run Firecracker.
  v0.13 lands it: see "Warm pool on microVMs — design" below, which replaces the refusal.
- **The use-case suite on firecracker is deferred.** `scripts/osb-usecases-e2e.sh` checks its cases
  against docker directly - `docker volume ls`, `docker ps --filter label=…`, `docker inspect` of
  the container - in most of them, so `--provider firecracker` is a port of the suite's
  verification layer, not a flag. v0.12's microVM gate is the unmodified upstream conformance tier
  (`scripts/osb-conformance.sh --tier v0.10.0 --provider firecracker`, CI `microvm` job, which now
  fails rather than skips without `/dev/kvm` and prewarms the large images) plus the Go microVM
  e2e. Porting the suite's checks to read provider state instead of docker's is v0.13.
- **Snapshots are disk-only.** Create-from-snapshot cold-boots a copy of the source's disk; no
  memory restore as a new sandbox. The fallback the Shape row above allowed "if that proves
  unsafe" is what shipped, because it did: the guest's IP lives in its memory and a memory clone
  carries every secret userspace made. See DECISIONS.md, "The OpenSandbox API on a microVM: the
  agent is PID 1, the token is the API's, a snapshot is the disk".

## Warm pool on microVMs — design (2026-09-26, v0.13)

**Member.** An ordinary API microVM of the pool key (image, entrypoint, limits, ports, platform,
idle mode — the docker `poolKey`), created born-running with execd healthy, then *parked*:

| mode | parked as | costs while waiting | claim |
|---|---|---|---|
| **asleep** (default) | Seal → Full snapshot → VMM ended | 0 RAM; disk ≈ VM RAM (`vm.mem`), shown in doctor | restore + re-key |
| **frozen** (`--osb-pool-freeze`) | VM paused, memory resident | RAM, no CPU | resume + re-key |

**Claim** is one provider call (`provider.PoolParker.Claim`), under the VM's lock: wake (restore
or resume), then one re-key carrying the API's fresh token, the caller's env and a new control
secret, at a new generation. The record's token becomes the caller's before the re-key, so every
later restore re-keys with it; a claimed VM that later dies awake and cold-boots is re-keyed after
the boot too, because its agent drive still carries the member's token and not the caller's.
**A member whose re-key fails is never handed out**: the VM is stopped, the claim errors, the API
discards the member (cause logged) and tries the next, then the cold path.

**Identity.** Each member is its own VM — own drives, slot, tap, token, control secret — booted
from its own disk. No member shares memory with another. *Fork-from-one-template is not this
release*: a memory clone carries every secret userspace minted before the snapshot and the
kernel-set guest IP (DECISIONS: "The OpenSandbox API on a microVM"), and re-key fixes only execd's.

**Refill** reuses the docker pool loop: `--osb-pool` concurrency, backoff on failure, quiet period
after claims; each member's boot is further bounded by the provider's `bootSlot` (host CPUs), and so
is each asleep claim's restore. Members stay invisible to list/get (record `Pool` set), held in the
daemon so traffic cannot wake one, pinned so the reaper leaves it, and removed on shutdown and on the
next start after a crash (the existing `recover`). Egress: members have no filter; a request with a
`networkPolicy` has no pool key and goes cold, as on docker. `--only` scoping is unchanged: members
are API sandboxes of this daemon.

**Refusals kept.** A RunsAgent provider without `PoolParker` (the helper-VM path) still refuses
`--osb-pool` at startup.

**Measured in CI, not here** (`microvm` job): create→first-command median/p95 cold vs pool, asleep
vs frozen, via `scripts/osb-bench.sh --pool` on firecracker; conformance subset (sandbox, command,
filesystem) with `SBX_OSB_POOL=python:3.11-slim=4`. Expectation from the spike: restore ~5 ms load
+ re-key (~170 ms nested; less on CI x86), resume ~3 ms + re-key — frozen should win on latency,
asleep on memory.
