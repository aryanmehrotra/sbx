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
| helper VM (Mac/Windows) | out of scope for this release: `--osb-addr` stays refused there with its current message. Linux direct first. |

## Out of scope
Mac/Windows helper-VM API; snapshot fork for pool speed; host volumes; the public gateway service.

## Test plan
1. Unit: every OSB route against a fake RunsAgent provider; firecracker provider against fcfake.
2. CI `microvm` job: `scripts/osb-conformance.sh --tier v0.10.0 --provider firecracker` + the use-case suite with `--provider firecracker` (cases that need docker-only features skip by name).
3. Local: the same inside a colima helper VM (arm64, nested) as a smoke test.
4. Must not regress: the docker tier 50/0/2, 15/15 use cases, Linux `-race`, the existing microVM e2e.

## Risks accepted
Snapshot files cost disk per pool member (≈ VM RAM each); cold create stays seconds until fork lands.
