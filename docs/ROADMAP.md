# Roadmap

> **What this page is:** what sbx builds next, why, and what it will not build. For users and
> contributors deciding whether to bet on sbx. As of **v0.14.0 (2026-09-27)**.

The wake path is the product. Next comes what makes it trustworthy for untrusted code and agents:
secrets kept out of the sandbox, the microVM measured on bare metal and proven on a Mac, and the
rest of the OpenSandbox API. Nothing here turns sbx into a hosted service.

Sizes are rough: **S** is under a week, **M** is 2–4 weeks, **L** is over a month, for one engineer.
They are estimates, not commitments, and there are no dates because they would be invented.
Competitor facts are sourced in [COMPARISON.md](COMPARISON.md).

## The rule this list is filtered through

sbx is one claim: **the connection is the wake-up call, it never gets refused, and idle costs
nothing.** An item earns a place here by making that claim truer: faster, on more workloads, or on
a stronger boundary. An item that only widens the surface does not, however good it would look in
a feature table.

Every item names the decision in [DECISIONS.md](DECISIONS.md) it answers to, or the gap it closes.
An item that can name neither is a wish, and belongs in an issue instead.

## Where we want to be best

sbx will not win every row of [the ranking](COMPARISON.md#honest-ranking). It aims to be first on
three, and holds itself to a bar it can measure.

| Dimension | Today | The bar |
|---|---|---|
| **Wake on connect** | 191 ms Redis on docker; 5/5 first connections served vs Lazytainer's 0/5 | every provider serves the first connection in CI, and a bare-metal microVM wake is published |
| **Idle cost on your hardware** | 0 B RAM asleep; the microVM's disk cost is unpublished | 0 B RAM on every provider, and `sbx doctor` reports each sleeper's disk cost |
| **Multi-service stacks, laptop to cluster** | one `sandbox.json` on docker, kubernetes and microVM; the microVM refuses `files`, `init`, host mounts | no everyday spec field refused on any provider without a documented reason |

## Shipped recently

Details are in each version's release notes, such as [v0.14.0](release-notes/v0.14.0.md).

- **v0.11.0** — the Firecracker microVM provider: own kernel, sleep keeps RAM and processes; a helper VM on M3+ Macs.
- **v0.12.0** — the OpenSandbox API served from microVMs, with `pvc` volumes and disk-snapshot forks.
- **v0.13.0** — a microVM warm pool, every VMM under Firecracker's jailer, and a host guard that fails closed.
- **v0.13.2** — upstream's `pool` and `e2e` conformance files pass; refusals no longer promise a release.
- **v0.14.0** — a network namespace per jailed VMM, a size limit on its writes, and a shared read-only root.

Also shipped in this period: live egress updates (`sbx egress`), CIDR and IP rules, and execd
sessions and background commands through the OpenSandbox API.

## Now

In progress or next up.

| Item | Why (competitive) | Size | Status |
|---|---|:---:|---|
| **Bare-metal microVM wake and burst numbers** | E2B, isorun and OpenSandbox ("~80ms" pools) publish microVM figures; sbx has only nested runs | S | harness exists: `SBX_FC_E2E=1 go test -run FirecrackerE2E ./internal/provider`; needs a bare-metal host |
| **The microVM's disk cost in `sbx doctor` and BENCHMARKS** | "0 B at rest" needs its asterisk: a snapshot is about the VM's RAM on disk. Vercel bills snapshot storage openly | S | not started |
| **The helper VM run end to end on a Mac and on Windows 11** | microsandbox and Docker Sandboxes run microVMs on all three OSes today | M | provider measured on an M4; the API through it is unit-tested only; Windows built, not run |
| **The warm pool through the helper VM** | burst on a Mac is otherwise the docker pool | M | not built |
| **Docs people can find** | E2B, Daytona, Modal and OpenSandbox have docs sites; sbx ranks last on adoption | S | docs overhaul in progress; a site is next |

## Next

| Item | Why (competitive) | Size | Status |
|---|---|:---:|---|
| **Secrets kept out of the sandbox** | OpenSandbox (vault), Vercel (brokering), isorun (proxy) and Cloudflare (header injection) have it; sbx does not | L | designed below |
| **Egress matchers** | path, method, header and query predicates on a rule; pure logic above `Permits()` | S | not started |
| **Isolated sessions** (`/v1/isolated/*`) | the biggest OpenSandbox gap: 48 upstream tests fail on it | not estimated | upstream's execd not yet read |
| **A code-interpreter guide** | E2B, Cloudflare, Daytona and OpenSandbox lead with one. sbx serves OpenSandbox's code-interpreter image already | S | the upstream `e2e` file passes; no guide |
| **Guides for Claude Code, Cursor and Codex**, and a GitHub Action | community growth: E2B's cookbook, microsandbox's examples and showcase | S each | [AI-AGENTS.md](AI-AGENTS.md) exists; no action |
| **`files`, `init` and host mounts into a microVM** | the multi-service bar above; the microVM refuses them today | M | refused by name |
| **Exec sessions on the CLI** | execd has sessions and background commands; `sbx exec` does not. Vercel's `runCommand` has `detached` | S | API shipped, CLI not |
| **The daemon off root; a quota on the state filesystem** | prerequisites before anonymous untrusted code on a shared host | M | open; see [SECURITY.md](../SECURITY.md) |

### Secrets kept out of the sandbox

Today an agent that calls an API needs the key in its environment, so it can exfiltrate it.
Allow-listing the domain does not help, because the key is still in the box.

The plan: terminate TLS at the egress filter and inject the credential there, so **the secret never
enters the sandbox**. The agent makes an unauthenticated request to an allowed host, and the filter
adds the header on the way out. OpenSandbox's credential vault API then sits on top of it.

The cost: a per-sandbox CA, leaf certificates minted on demand, and that CA installed into each
image's trust store. Postgres needs its own path, because it negotiates TLS after the TCP
connection is up. Roughly 2,000 lines and a threat model to defend in [SECURITY.md](../SECURITY.md).

**It does not weaken the allow-list.** Only domains with a rule that needs it are terminated;
everything else is spliced on the SNI as today, undecrypted. The filter's design is in
[DECISIONS.md](DECISIONS.md#a-live-egress-policy-is-held-by-the-filter-and-pushed-to-it) and
[DECISIONS.md](DECISIONS.md#default-allow-is-enforced-by-the-same-door-and-it-costs-raw-tcp).

## Later

| Item | Why | Size |
|---|---|:---:|
| **Snapshot retention** (expiry, keep-last-N) | a long-lived branch should not accumulate; Vercel has `keepLastSnapshots` | S |
| **Reusable volumes with a lease** | `pvc` volumes outlive a sandbox; missing is single-writer leasing for a shared cache | M |
| **Egress filtering on kubernetes** | a cluster refuses `egress_policy` today; the answer is a NetworkPolicy plus an egress gateway | S–M |
| **Forking a microVM's memory under a new name** | E2B and microsandbox fork live sandboxes; sbx forks a VM's disk | M |
| **A local Go API** | a test harness drives sandboxes in-process. **Local only**, see below | S–M |
| **`devcontainer.json` import out of Preview** | Coder, Ona and DevPod read it natively | S |
| **Deploy recipes for your own VM or cluster** | a one-command path to *your* cloud; not a hosted sbx | S |

## Not built yet in the OpenSandbox API

When the OpenSandbox API or execd answers `501 … not built yet (docs/ROADMAP.md)`, the feature is
listed here. Each is refused by name, never approximated, as the
[OpenSandbox decisions](DECISIONS.md#the-opensandbox-api-on-a-microvm-the-agent-is-pid-1-the-token-is-the-apis-a-snapshot-is-the-disk)
require.

| Refused feature | What it is | Size |
|---|---|:---:|
| **isolated sessions** (`/v1/isolated/*`) | an execd session with its own PID namespace, `/tmp` and overlay | not estimated |
| **credentialProxy** · credential vault | credentials injected into matching outbound requests | L, on the brokering above |
| **server-side pools** (`extensions.poolRef`) | a pool the server owns, named at create. `--osb-pool` serves unnamed creates | not estimated |
| **secureAccess** · **signed endpoints** (`?expires=`) | endpoints that need a signature or token | not estimated |
| **server-proxied endpoints** (`use_server_proxy=true`) | execd reached through the lifecycle server | not estimated |
| **registry credentials in image.auth** | pull credentials in the create request; today, `docker login` on the host | not estimated |
| **lifecycle hooks** · **renew-on-access** | hooks around lifecycle events; an expiry extended by each access | not estimated |
| **the API on kubernetes** | needs an init-container `Injector` to put execd into any image | M |

Network policies answer 501 for a different reason: the daemon was started without egress control.

Conformance against upstream's `tests/go` (release-1.1.0, docker, CI, 2026-09-27): every v0.10.0-tier
file passes, and upstream's `pool` and `e2e` files are gated in CI. The v0.11.0 tier was 70 passed,
48 failed (all `isolated_session`), 6 skipped; `credential_vault` skips for want of a target host.

## Not doing (and why)

A roadmap that only lists additions is a wish list. These are ruled out by an existing decision.

| | Why |
|---|---|
| **Hosting sbx for anyone** | [*sbx is a tool people run, not a service anyone offers*](DECISIONS.md#sbx-is-a-tool-people-run-not-a-service-anyone-offers). This is why sbx ranks last on "hosted option", and it stays last. Deploy recipes for your own infrastructure are fine |
| **Auth, tenancy, quotas, per-user tokens** | Same decision. Once sbx asks "who are you" rather than "is this yours", it has become that service. The answer is a gateway in front |
| **A remote control plane** | Same decision. `create`, `rm` and `exec` stay local. `sbx connect` is a data-plane tunnel. An SDK that creates sandboxes over the network is this item in a different hat, which is why the Go API is local |
| **A browser IDE** | `sbx ssh` gives VS Code, Cursor and JetBrains a Remote-SSH path over the wake path. Codespaces, Coder and Ona win here, deliberately |
| **Per-agent users inside one sandbox** | two agents get two sandboxes: one agent's writes cannot reach the other's |
| **A second VMM on Apple's Virtualization.framework** | it cannot snapshot for third parties and would cost cgo and the static binary. [DECISIONS.md](DECISIONS.md#a-microvm-off-linux-runs-in-a-helper-vm-not-on-virtualizationframework) |
| **Kubernetes inside a docker sandbox** | k3s and docker-in-docker need `seccomp=unconfined` or `privileged`, which sbx does not offer. Not yet tried on the microVM provider |

## Decisions for the owner

- **The name.** Docker Sandboxes also ships a CLI called `sbx` (`brew install docker/tap/sbx`).
  Keep the name with a disambiguation line, or add a distinct package name or binary alias. This
  is the owner's call, not the roadmap's.

## How something gets on this list, or off it

**On:** it makes the wake path faster, correct on a workload where it is not, or safe on a
boundary where it is not. It names the decision it answers to. Its size comes from someone who has
read the code it touches.

**Off:** it shipped, or it was measured and did not pay. A shipped item moves to the release notes.
A rejected one moves to [DECISIONS.md](DECISIONS.md) with the measurement that killed it. Nothing is
quietly deleted, because the reason an item failed is worth more than the item was. The previous
roadmap, including the microVM plan and its estimates, is in git history at v0.14.0.
