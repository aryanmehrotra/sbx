# Roadmap

> **Short version:** the wake path is the product, and the next big piece of work makes it
> **7–45× faster while bringing memory and running processes back with it** — a microVM provider
> where `Start` is a snapshot restore rather than a cold boot. Everything else here is the egress
> filter growing into a real network policy. Nothing here turns sbx into a hosted service.

Estimates are in engineer-weeks and they are estimates, not commitments. Ordering is deliberate;
dates are not given because they would be invented.

Every item names the decision in [DECISIONS.md](DECISIONS.md) it answers to. An item that cannot
name one is a wish, not a plan, and belongs in an issue instead.

---

## The rule this list is filtered through

sbx is one claim: **the connection is the wake-up call, it never gets refused, and idle costs
nothing.** An item earns a place here by making that claim truer — faster, on more workloads, or
on a stronger boundary. An item that only widens the surface does not, however good it would look
in a feature table.

That is why the list below is short and why the last section is as long as the rest.

---

## 1 · A microVM provider

**The big one.** Today a sleeping sandbox is a stopped container with its volume intact, so a wake
is a cold process start against warm data: 191 ms for redis, 931 ms for postgres, and Postgres
replays its WAL on the way back. That is the right trade against `docker start`, and it is not the
best trade available.

A microVM's snapshot restore brings back **memory and running processes**, not just the disk:

| | today, docker | with a microVM provider |
|---|---|---|
| wake | 191 ms redis · 931 ms postgres | **4–28 ms**, workload-independent |
| what comes back | disk warm, process **cold** | **RAM + processes, already running** |
| boundary | namespaces, host kernel | **dedicated guest kernel** |
| memory at rest | 0 B | 0 B — the image is on disk, not resident |

The wake stops being a function of the workload. A Postgres that took 931 ms to replay its WAL
comes back in the same few milliseconds as a redis, because nothing starts — it resumes.

### Why it fits rather than fights

Four things about the current design make this an addition and not a rewrite:

| | |
|---|---|
| **`Provider` is an interface, and has been since the cluster backend** | A third implementation is the sanctioned extension point. Docker and kubernetes already prove the interface is not shaped like either one. |
| **Firecracker's control plane is REST over a unix socket** | A stdlib `http.Client` with a unix `DialContext` drives it. **`go.mod` stays at zero dependencies** — no VMM SDK, no cgo. This is the constraint that decides the whole design; see the macOS section for where it bites. |
| **Capabilities are negotiated, not stubbed** | `Snapshotter` is already optional. A microVM backend implements it *natively and better*, because its snapshot includes memory — which is the one thing `sbx snapshot` cannot do today outside CRIU on podman. |
| **Isolation already fails closed and says why** | `sbx doctor` has the refusal machinery. A provider the machine cannot run is refused in one second with a reason, which is the behaviour this needs on day one. |

The two-port wake proxy does not change at all. It splices bytes and has no opinion about what is
upstream of it.

### The work

| | | est |
|---|---|---|
| **VMM driver** | boot-source, drives, network-interfaces, machine-config, actions, snapshot create/load, over the unix socket | 2 wk |
| **OCI → rootfs pipeline** | pull, apply layers in userspace honouring `.wh.` whiteouts, build ext4, cache behind a `.built` sentinel with atomic rename, clone per-VM with `FICLONE` | **4 wk** |
| **Guest kernel** | pin one per architecture; unwrap EFI zboot images (`MZ` at 0, `zimg` at 4) | 2 wk |
| **Guest agent** | a static musl binary on `AF_VSOCK` speaking a framed protocol — this is what backs `Exec`, `ExecTTY`, `Copy` and `Logs` | 3 wk |
| **Networking** | a tap per VM on a bridge. The no-NAT bridge that `egress: "deny"` already uses maps straight over | 2 wk |
| **Wake path** | `Start` becomes snapshot-load, `Stop` becomes snapshot-create. The whole point of the exercise | 2 wk |
| **doctor, refusals, tests** | to the bar the repo already holds itself to | 3 wk |

**≈ 18 weeks on Linux, one engineer.** The rootfs pipeline is the bulk and the part to spike
first; the VMM driver is the part that looks hard and is not.

### Two landmines, written down before anyone hits them

**vsock modules must vermagic-match the kernel exactly.** `vsock`,
`vmw_vsock_virtio_transport_common` and `vmw_vsock_virtio_transport` have to be loaded before
boot, and a version mismatch **fails silently** — the guest boots, and the agent simply never
connects. That reads like a broken agent and is a kernel packaging problem. Budget the debugging
there, not in the VMM.

**A restored snapshot is not a fresh one.** Resuming the same memory image more than once reuses
whatever the guest had already generated: entropy pools, session identifiers, anything derived
from them. That is fine for `resume` and wrong for `fork`, so a forked VM has to re-seed rather
than simply start from the parent's image.

### macOS: pick A, then B, and do not build C

Firecracker needs KVM. That makes the host question the real decision in this item, not the VMM
one.

| | | cost |
|---|---|---|
| **A · Linux only, refused elsewhere** | `--provider microvm` is refused on darwin, and `sbx doctor` says why. Exactly the behaviour `--isolation gvisor\|kata` already has | **+0** |
| **B · nested virtualisation** | M3-and-later on macOS 15+ exposes a real `/dev/kvm` inside a Linux VM, which runs Firecracker unmodified | +2–3 wk |
| **C · a second VMM on Apple's Virtualization.framework** | a second backend behind the same provider | +8–12 wk, **and it does not work** |

**C is a trap and the reason is specific.** Virtualization.framework cannot snapshot: its own
`validateSaveRestoreSupport` reports success, and then `saveMachineStateToURL` fails with a
generic internal error, because the entitlement it needs is restricted to Apple's own
applications. So the option costs cgo — and with it the pure-Go static binary that is half of
what people install this for — and then does not deliver the fast resume the whole item exists to
get. It is on this list only so that nobody spends two months rediscovering it.

**A ships the provider. B is the macOS story. C stays unbuilt.**

### The honest trade

**"0 B at rest" survives; "costs nothing at rest" gets an asterisk.** A snapshot is a memory image
on disk, roughly the size of the VM's RAM. No resident memory, but no longer free either — and a
fleet of twenty sleeping sandboxes now has a disk number attached to it where before it had a
volume and nothing else. That belongs in `sbx doctor` and in BENCHMARKS.md when it lands, not in a
footnote.

---

## 2 · The egress filter becomes a network policy

`egress_allow` today is a host suffix list, fixed when the service is created, enforced by a
CONNECT proxy on a bridge with no route out. The mechanism is right — a filtering proxy in the
data path, a component with a lifecycle — and the policy it can express is thin.

| | | est |
|---|---|---|
| **Live updates** | change the list on a running service without recreating it. The filter is already a long-lived container with an activity hook; the list is the only thing frozen. Lets a box fetch its dependencies wide open, then lock down before untrusted work starts — which currently needs two sandboxes | 2–3 d |
| **Matchers** | path, method, header and query predicates on a rule. Pure logic above `Permits()` | 2 d |
| **CIDR allow and deny** | address rules for traffic that has no hostname to match on. Not a proxy change — the bridge has to enforce it, or a client that dials an IP directly walks around the list | 1 wk |
| **A terminating mode, and credential brokering on top of it** | see below | 4–8 wk |

### Credential brokering is the one worth the money

The problem it solves is one sbx has no answer to today: an agent needs to authenticate to an API,
so the key goes in the sandbox's environment, so the agent can exfiltrate it. Allow-listing the
domain does not help — the key is still in the box.

Terminating TLS at the filter and injecting the credential there means **the secret never enters
the sandbox at all.** The agent makes an unauthenticated request to an allowed host; the filter
adds the header on the way out.

What it costs: a per-sandbox CA, leaf certificates minted on demand, and that CA installed into
each image's trust store with the environment variables that make the common runtimes honour it.
Postgres needs its own path because it negotiates TLS after the TCP connection is up. Call it
2,000 lines and a threat model to defend in [SECURITY.md](../SECURITY.md).

It also unlocks forwarding a matched request to a proxy the operator controls, which is the
mechanism for restricting a domain to particular paths rather than all of it.

**This does not weaken the allow-list.** Termination happens only for domains carrying a rule that
needs it. Everything else is spliced on the SNI as it is today, undecrypted.

---

## 3 · Smaller, and unblocked

| | | est |
|---|---|---|
| **Exec sessions** | detached commands, streamed output, exit codes carried back. `Exec` and `ExecTTY` exist; what is missing is a session that outlives one call | 1 wk |
| **A local Go API** | the daemon's own package, importable, so a test harness can drive sandboxes in-process instead of shelling out. **Local only** — see the exclusions below | 1–2 wk |
| **Reusable volumes** | a named volume that outlives the sandbox that made it, single-writer, for a dependency cache several sandboxes take turns on. Docker volumes already do the storage; what is missing is the lease and the lifecycle | 2–3 wk |
| **Snapshot retention** | an expiry and a keep-last-N on `sbx snapshot`, so a long-lived branch does not accumulate | 1 wk |
| **`egress_allow` on kubernetes** | the field is docker-only. A cluster expresses it as a NetworkPolicy, natively | 1–2 wk |

---

## What this deliberately does not include

A roadmap that only lists additions is a wish list. These are ruled out, and by an existing
decision rather than by this document.

| | why |
|---|---|
| **Auth, tenancy, quotas, per-user tokens** | [DECISIONS.md — *sbx is a tool people run, not a service anyone offers*](DECISIONS.md). The line is written there: the moment sbx grows something answering "who are you" rather than "is this yours", it has become the thing that section rules out, and the answer is a gateway in front rather than an identity system inside. |
| **A remote control plane** | Same decision. `create`, `rm` and `exec` stay local-only. `sbx connect` is a data-plane tunnel with one token proving you own the deployment, and it stays that. **An SDK that creates sandboxes over the network is this item wearing a different hat** — which is why the local API above is scoped as local. |
| **Hosting it for anyone** | Same decision. "Somebody else runs it" is a different product and sbx is run by you. |
| **A browser IDE** | Still none, still not planned. `sbx ssh` reaches a sandbox with VS Code Remote-SSH over the wake path that already exists. |
| **Per-agent users inside one sandbox** | sbx's answer to two agents is two sandboxes, and that answer is better: one agent's writes cannot reach another's, which user separation inside a shared box does not give you. |
| **Kubernetes inside a sandbox** | k3s and docker-in-docker want `seccomp=unconfined` or full `privileged`, and sbx offers neither. `cap_add` was the missing mechanism and it is not sufficient. The microVM provider changes this question completely — revisit it there, not here. |

---

## How something gets on this list, or off it

**On:** it makes the wake path faster, or correct on a workload where it is not, or safe on a
boundary where it is not. It can name the decision it answers to. Its cost is estimated in weeks
by someone who has read the code it touches.

**Off:** it shipped, or it was measured and did not pay. Both leave a trace — a shipped item moves
to the release notes, a rejected one moves to [DECISIONS.md](DECISIONS.md) with the measurement
that killed it. Nothing is quietly deleted, because the reason an item failed is worth more than
the item was.
