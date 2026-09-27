| **`egress_policy`** · live `sbx egress` · CIDR rules | ● | ○ | a filtering proxy on a no-NAT bridge; a cluster needs a NetworkPolicy plus an egress gateway, and refuses rather than approximates |
# Compared

> **Short version:** sbx wakes on any TCP connection — no SDK, no account, on hardware you own.
> That corner of the sandbox landscape is sbx's alone. Every figure here is measured in this
> repo or quoted from the vendor with a link — never from a competitor's marketing.

## The whole field, one table

● yes · ◐ partial or conditional · ○ no · – not checked. The rows below the fold break each of these down and
say where the ◐ and ○ are choices rather than gaps.

| | sbx | E2B | Daytona | Modal | Cloudflare | Fly | Neon | Northflank |
|---|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|
| Wakes on a raw socket | ● | ○ | ○ | ○ | ○ | ◐ ded. IPv4 | ◐ pg only | ○ |
| Runs on your laptop | ● | ○ | ○ | ○ | ○ | ○ | ○ | ○ |
| Same spec local + cluster | ● | ○ | ○ | ○ | ○ | ○ | ○ | ◐ |
| Self-hosted, no account | ● | ◐ OSS infra | ◐ OSS core, stale | ○ | ○ | ○ | ○ | ◐ BYOC |
| Arbitrary stateful services | ● | ◐ | ● | ◐ | ◐ | ● | ○ pg | ● |
| Multiple services, one spec | ● | ○ | ○ | ○ | ○ | ◐ | ○ | ● |
| Zero cost at rest | ● | ◐ storage | ◐ storage | ◐ storage | ◐ storage | ◐ storage | ◐ storage | ○ |
| RAM-state snapshot | ● microVM | ● | ◐ VM sandboxes | ◐ alpha | ○ | ● | n/a | ○ |
| VM-grade isolation | ● microVM, jailed | ● | ◐ | ◐ gVisor | ● | ● | ● | ● |
| Speaks the OpenSandbox API | ● | ○ | ○ | ○ | ○ | – | – | – |
| Public URL per sandbox | ● | ● | ● | ● | ● | ● | n/a | ● |
| GPU | ◐ docker | ◐ | ◐ | ● | ○ | ● | n/a | ● |

Re-verified 2026-08-31, and two cells moved. Fly's raw-socket wake is conditional: HTTP goes
through Fly Proxy freely, but raw TCP needs a dedicated IPv4 and is unreliable on a shared one -
so ◐, not ●. And Daytona's open-source core has been unmaintained since June 2026 with development
moved to a private codebase, which makes "self-hosted" true of the repo and not of the product.

Re-verified 2026-09-27 against each vendor's own pages, and six cells moved:

- **sbx, both isolation rows.** `--provider firecracker` (v0.11–v0.13) gives each sandbox its own
  guest kernel, a VMM under Firecracker's jailer, and a sleep that keeps memory and running
  processes. It needs Linux with `/dev/kvm`, or an M3+ Mac through a helper VM. On docker it is
  still a container.
- **Cloudflare, RAM-state ○:** on sleep "all processes terminate", and a backup is a directory
  in R2 ([sandboxes][cf-sb], [backup][cf-bk]).
- **Modal, isolation ◐:** gVisor, a user-space kernel rather than a VM ([security][modal-sec]).
  Its memory snapshots are marked alpha ([snapshots][modal-snap]).
- **Daytona, RAM-state ◐:** memory is kept only for VM sandboxes; the default is a container
  ([sandbox][daytona-sb]).
- **E2B, self-hosted ◐:** its infrastructure is Apache-2.0 and can run in your own cloud
  ([e2b-dev/infra][e2b-infra]); the product most people use is hosted.

The **OpenSandbox API** row is new. None of E2B, Daytona, Modal or Cloudflare says it speaks that
API (Fly, Neon and Northflank were not checked); sbx implements it
to upstream's own test suite, with the gaps in the next section.

[cf-sb]: https://developers.cloudflare.com/sandbox/concepts/sandboxes/
[cf-bk]: https://developers.cloudflare.com/sandbox/guides/backup-restore/
[modal-sec]: https://modal.com/docs/guide/security
[modal-snap]: https://modal.com/docs/guide/sandbox-snapshots
[daytona-sb]: https://www.daytona.io/docs/en/python-sdk/sync/sandbox/
[e2b-infra]: https://github.com/e2b-dev/infra

Worth knowing about the two nearest: Cloudflare sleeps a sandbox after 10 idle minutes and its
filesystem is ephemeral - a restart comes back from the image, and persistence is an opt-in R2
backup rather than a mounted volume - while E2B's pause genuinely preserves memory and running
processes. Neon is the only one on this page that wakes on an unmodified client connection like
sbx does, and only for Postgres.

**Read the top four rows first.** Waking on a raw socket, running on your own laptop, the same
spec from laptop to cluster, self-hosted with no account — that combination is sbx's alone here.
The microVM and GPU rows are opt-in (`--provider firecracker`, docker GPU passthrough): there
when you want them, out of the way when you don't.

---

## Agent sandboxes: the OpenSandbox API and the platforms it is weighed against

Since v0.9 sbx also answers the other meaning of "sandbox" (next section): a box an agent's code
runs in, driven over an API. Here it stands beside the reference server for that API and the
hosted platforms agents use. Every vendor cell is quoted from the vendor's own page, read
**2026-09-27**; "NP" means not published.

| | **sbx** | OpenSandbox | E2B | Daytona | Modal | Cloudflare | Vercel | isorun |
|---|---|---|---|---|---|---|---|---|
| isolation | container, or Firecracker microVM (jailed) | container, gVisor, Kata or Firecracker [¹][osb] | Firecracker [²][e2b] | container; VM optional [³][dt-sb] | gVisor [⁴][modal-sec] | container in its own VM [⁵][cf-arch] | Firecracker [⁶][vc] | KVM [⁷][iso] |
| published create figure | see below | create → Ready p50 97 ms serial, 225 ms at 10-way [⁸][osb-perf] | NP | "sub 90ms … code to execution" [⁹][dt-price] | "< half a second" median, create → can run code [¹⁰][modal-1m] | 1–3 s from stopped [⁵][cf-arch] | "milliseconds", no number [⁶][vc] | "10ms", undefined [⁷][iso] |
| pause keeps RAM + processes | ● microVM · ○ docker | ● resume 1.0 s local [⁸][osb-perf] | ● ~1 s resume [¹¹][e2b-p] | ◐ VM sandboxes only [³][dt-sb] | ◐ alpha [¹²][modal-snap] | ○ processes end [¹³][cf-sb] | ○ filesystem only [¹⁴][vc-p] | ● same PIDs [¹⁵][iso-l] |
| self-host · licence | ● your machine · MIT | ● Apache-2.0 [¹][osb] | ◐ Apache-2.0 infra [¹⁶][e2b-infra] | ◐ AGPL-3.0, unmaintained since 2026-06 [¹⁷][dt-gh] | ○ | ○ SDK only | ○ SDK only | ○ SDK only (MIT) |
| OpenSandbox API | ● to v0.10.0 tier ([ROADMAP §4](ROADMAP.md)) | ● the reference | ○ | ○ | ○ | ○ | ○ | ○ |
| MCP server | ● `sbx mcp` | ● | ● | ● | NP | NP | NP | ● |
| code interpreter | ● | ● | ● | ● | NP | ● | NP | NP |
| egress policy | ● hosts, wildcards, CIDRs, live | ● | ● domain/CIDR, live [¹⁸][e2b-net] | ◐ tier-limited [¹⁹][dt-net] | ● CIDR; domains beta [²⁰][modal-net] | ● HTTP [²¹][cf] | ● domain/CIDR, live [²²][vc-fw] | ● [⁷][iso] |
| credentials kept out of the box | ○ roadmap | ● vault [¹][osb] | NP | NP | NP | ◐ header injection [²¹][cf] | ● brokering [²²][vc-fw] | ● proxy [⁷][iso] |
| pricing | $0, your hardware | $0, your hardware | per vCPU-s + GiB-s | per vCPU-h + GiB-h | per core-s + GiB-s | per active vCPU-s | per active CPU-h + per create | per vCPU-h + GiB-h |

### Create → first command, measured the same way

A vendor's "create" figure is whatever that vendor chose to time, so the row above does not
compare. The one number that does is **ComputeSDK's Burst TTI**: 100 concurrent creates, each
timed from the request to a successful `node -v`, run daily from the same runner
([methodology][csdk-m]). Latest run, 2026-09-25 ([results][csdk-r]):

| | median TTI | | median TTI |
|---|---:|---|---:|
| isorun (#1) | 43.6 ms | Vercel | 452.9 ms |
| Daytona | 341.4 ms | Cloudflare | 647.8 ms (5,270 ms on 2026-09-18) |
| Modal | 907.6 ms | E2B | 1,237.7 ms |

**sbx is not on it, and OpenSandbox is not either.** The leaderboard benchmarks hosted services
with credentials handed to it; sbx is a tool you run, so there is nothing to hand over yet. What
sbx has is the same shape run by its own scripts ([BENCHMARKS.md](BENCHMARKS.md)), which is not a
leaderboard entry:

| sbx, same shape | median | where |
|---|---:|---|
| docker, warm pool, 1 at a time | 13.7 ms | Apple M4, colima |
| docker, warm pool, 100 at once | 472.1 ms | Apple M4, colima |
| microVM, frozen pool, 4 at once | 141 ms | GitHub runner, nested KVM |
| microVM, cold, 4 at once | 2,821 ms | GitHub runner, nested KVM |

Different machines, different concurrency, no network in between. Read them as "what sbx does on
this hardware", not as a rank.

[osb]: https://github.com/alibaba/OpenSandbox
[osb-perf]: https://github.com/alibaba/OpenSandbox/blob/main/docs/architecture/fast-sandbox/performance.md
[e2b]: https://e2b.dev/
[e2b-p]: https://docs.e2b.dev/sandbox/persistence
[e2b-net]: https://docs.e2b.dev/sandbox/internet-access
[dt-sb]: https://www.daytona.io/docs/en/sandboxes
[dt-price]: https://www.daytona.io/pricing
[dt-gh]: https://github.com/daytonaio/daytona
[dt-net]: https://www.daytona.io/docs/en/network-limits/
[modal-1m]: https://modal.com/blog/scaling-to-1-million-concurrent-sandboxes-in-seconds
[modal-net]: https://modal.com/docs/guide/sandbox-networking
[cf]: https://developers.cloudflare.com/sandbox/
[cf-arch]: https://developers.cloudflare.com/containers/platform-details/architecture/
[vc]: https://vercel.com/docs/sandbox/concepts
[vc-p]: https://vercel.com/docs/sandbox/concepts/persistent-sandboxes
[vc-fw]: https://vercel.com/docs/sandbox/concepts/firewall
[iso]: https://isorun.ai/
[iso-l]: https://docs.isorun.ai/sandboxes/lifecycle
[csdk-m]: https://github.com/computesdk/benchmarks/blob/master/METHODOLOGY.md
[csdk-r]: https://github.com/computesdk/benchmarks/blob/master/results/burst_tti/latest.json

---

## First, two different things are called "sandbox"

The platforms people compare this to mostly aren't doing the same job. The word covers two
products that share almost nothing:

```
   A PLACE TO RUN CODE                    A PLACE TO RUN THE SERVICES
                                          YOUR BRANCH NEEDS
   ───────────────────────────            ───────────────────────────────
   unit    an execution session           unit    a long-lived set of ports
   client  an SDK you call                client  psql, redis-cli, a pool,
   sold on isolation                              Playwright, a test runner
   dies    when the task ends             sold on cost at rest
                                          lives  as long as the branch

   E2B · Daytona · Modal                  sbx · Neon · preview-env platforms
   Vercel Sandbox · Cloudflare
```

**sbx is the right-hand column.** For untrusted model-authored code that needs a kernel boundary,
the left-hand column is what those platforms are built for; sbx's opt-in `--isolation gvisor|kata`
covers some of that ground, but the everyday case here is a long-lived service, not a single
execution.

## The axis that actually separates them

Everybody scales to zero. The question nobody's table asks is **what has to happen to bring it
back**, because that decides who is allowed to be the client.

| | wakes on | so the client can be | runs off-cloud |
|---|---|---|---|
| **sbx** | **any TCP connection** | anything with a socket | **laptop + cluster** |
| E2B | `sandbox.connect()` — an SDK call | only your own code | ✗ hosted |
| Daytona | start/resume API call | only your own code | ✗ hosted |
| Modal | SDK call | only your own code | ✗ hosted |
| Vercel Sandbox | SDK call | only your own code | ✗ hosted |
| Cloudflare Sandbox SDK | SDK call over RPC | only your own code | ✗ hosted |
| Fly Machines | a request through Fly Proxy | HTTP freely; raw TCP needs a dedicated IPv4 | ✗ their proxy |
| Knative | an HTTP request through the activator | HTTP/gRPC/WebSocket only | ✓ needs a cluster |
| Neon | a Postgres connection | Postgres clients only | ✗ hosted |

A connection pool cannot call `sandbox.connect()`. Neither can `psql`, `pg_dump`, a migration
tool, Playwright over CDP, or a test runner someone else wrote. On any SDK-triggered platform
*something in your code* has to know the sandbox is asleep and wake it first — so every tool that
touches it must be sandbox-aware. Here the socket is the signal, and the tools stay unmodified.

Two get this right and are worth naming: **Fly Proxy** wakes on a real connection and supports TCP;
**Neon** wakes on the Postgres protocol itself. Both are hosted and excellent — the ideas here are
not novel against them. What's different is that this runs on your laptop, no account, any protocol.

## What "asleep" costs, and what it keeps

| | at rest | wake | what survives |
|---|---|---|---|
| **sbx** | **0 B RAM**, and the volume it had | **191 ms** redis · 931 ms postgres · 1534 ms k8s | disk; processes cold-start |
| E2B | storage only | **~1 s** | **disk + RAM + running processes** |
| Daytona | storage only | *no wake figure published* | disk, persistent volume |
| Fly (suspended) | storage only | a few hundred ms | RAM snapshot |
| Fly (stopped) | storage only | ~2 s+ | disk |
| Neon | storage only | a few hundred ms | Postgres data |
| Knative | 0 pods | pod schedule, seconds | volume, if attached |

sbx does not snapshot RAM by default: a sleeping sandbox is a stopped container with its volume
intact, so a wake is a **cold process start against warm data** — Postgres replays its WAL and
comes back up. That's why sleeping costs nothing to *enter*: no RAM image to write, no meter on
the disk it keeps. For a database on a branch, disk-warm is the state that matters.

For workloads where memory has to survive the sleep — an agent's half-finished REPL, a redis with
no on-disk persistence — `sbx checkpoint` / `sbx resume` use CRIU to save and restore a running
process's memory, **proven end to end on a Linux podman runtime**: a redis key set only in memory
survives a freeze/resume. It needs Linux (refused on macOS) and a podman runtime, because docker's
own checkpoint/restore is broken; `sbx snapshot`/`fork` stay filesystem-only for now.

## What sbx does not have

Written down because a comparison page that only lists wins is an advertisement.

| | state |
|---|---|
| A hosted web IDE | **none, and not planned.** `sbx ssh` (Preview gate) reaches a sandbox with VS Code Remote-SSH over the wake path that already exists, and `sbx init --from-devcontainer` reads the `devcontainer.json` a repo already has - but there is no browser editor, and every dev-environment product ships one |
| Memory restore | **podman on Linux only**, via `sbx checkpoint` / `sbx resume`. On docker the restore is docker's own bug, not sbx's. Everywhere else it is filesystem snapshots and a cold start |
| Kubernetes in a sandbox | still no. `cap_add` now lets a spec ask for named capabilities, which was the missing mechanism - but k3s and DinD want `seccomp=unconfined` or full `privileged`, and sbx offers neither. Measured: CRIU clears its capability check with `CAP_SYS_ADMIN` and then fails on a `mount` the default seccomp profile filters |
| Windows | WSL2 only - sbx cannot dial a Windows named pipe. The **test suite** did not compile there either until v0.8.0, which CI now catches |
| A pooled upstream | the daemon dials a fresh upstream socket per client connection. Fine for the clients sbx fronts; it is the ceiling under extreme churn |

Three rows left this table in v0.8.0, and are listed here rather than quietly deleted:
**`egress_allow` on a VM-backed docker** (the filter now runs as a container on the bridge, so it
works on every Mac instead of being refused), **a waiting page** (shipped behind
`SBX_FEATURES=waiting-page`; it was a spinner before, and Sablier's is the one it was measured
against), and **a half-close on `sbx connect`** (it silently truncated the reply before).

## The closest prior art

Not the hosted platforms — the self-hosted projects that solve the same problem, and the ones
to read before this one. Two predate it.

**zeropod** · [ctrox/zeropod](https://github.com/ctrox/zeropod) · 941★ · **measured.** A
containerd shim: eBPF watches TCP activity, CRIU checkpoints the container to disk after idle, and
an activator **restores on the first connection in tens to a few hundred ms** — with memory, open
files and processes intact. Same mechanism as ours, one layer down: **272 ms median, n=4, 4/4
first attempts served**, measured against this repo (`scripts/zeropod-probe.sh`, CI). It replaces
your container runtime — a containerd shim, CRIU and eBPF on every node, plus a cluster; its
README calls arm64 in a Linux VM on macOS "somewhat flaky", i.e. Docker Desktop on Apple Silicon.
sbx is a userspace binary you run as yourself, with no runtime to replace.

**Sablier** · [sablierapp/sablier](https://github.com/sablierapp/sablier) · 2.9k★. The most
popular and the most different: an API server that reverse proxies (Traefik, Caddy, Nginx, Envoy,
APISIX, Istio) call through middleware, over five providers (docker, swarm, podman, kubernetes,
Proxmox LXC), with a blocking strategy or a themed waiting page. **It is HTTP-only** — the wake is
a middleware hook on an HTTP request, so nothing wakes `psql` without a proxy that speaks the
Postgres wire protocol, which is the problem it declines to solve. Steady-state overhead is
~1.5–2 ms/request vs our ~15 µs, because a proxy that parses is not a proxy that splices. Worth
stealing: the waiting page — `sbx url` currently offers only a spinner.

Re-checked 2026-08-30: still HTTP-only in v1.17.0. Two things have moved since — a "scale mode"
that throttles CPU and memory instead of stopping, and Proxmox LXC as a provider. And there is
now an unofficial [vbrandl/sablier-proxy](https://github.com/vbrandl/sablier-proxy), a generic
TCP proxy that calls Sablier's API to start a container on any connection — a third party at 7
stars, not part of sablierapp/sablier, so the HTTP-only verdict stands for Sablier itself. It is
named because the gap is real enough that somebody went and filled it.

**Lazytainer** · [vmorganp/Lazytainer](https://github.com/vmorganp/Lazytainer) · 758★. Counts
packets on an interface and stops containers below a threshold (default 30). Protocol-agnostic
like ours and closest in spirit — but containers must be labelled into a group **and have their
traffic routed through the Lazytainer container**, so it owns your networking; a packet count
means a chatty health check keeps things awake and a quiet session looks idle. No spec file, no
per-service readiness, no exports.

| | wakes on any TCP | no runtime replacement | laptop-first | per-service spec | restores RAM |
|---|---|---|---|---|---|
| **sbx** | ● | ● | ● | ● | ○ |
| zeropod | ● | ○ shim + CRIU + eBPF | ○ | ○ | ● |
| Lazytainer | ● | ◐ owns networking | ● | ○ | ○ |
| Sablier | ○ HTTP only | ● | ● | ◐ labels | ○ |
| KubeElasti | ○ HTTP/gRPC (unverified) | ● | ○ | ○ | ○ |

**KubeElasti** · [truefoundry/KubeElasti](https://github.com/truefoundry/KubeElasti) · 268★ · the
newest entrant, and named here so it is ruled out explicitly rather than left findable. Two modes:
Proxy Mode intercepts and queues requests while a Deployment sits at zero replicas, Serve Mode
gets out of the way once it is up. Kubernetes only, no laptop story, no spec file, and a cold pod
schedule rather than a memory restore. Its docs are written in request-and-queue language
throughout, which reads as HTTP/gRPC rather than raw TCP - **not verified**, because the page that
would say so is missing and the README does not state it. Either reading leaves the intersection
below unoccupied, so the claim does not rest on which one is right.

**What's unoccupied is the intersection: arbitrary TCP, no runtime to replace, a committed spec
file, and the same binary on a laptop and in a cluster.** zeropod goes deeper on memory restore
but needs a cluster; Lazytainer is protocol-agnostic but routes your traffic through it; Sablier
covers HTTP and is the one most people already run. Each is worth reading before this one.



## What the alternatives cost, for one developer

One environment of roughly 2 vCPU and 4 GB, used eight hours a day, twenty days a month — 160
hours. Rates read from each vendor's own pricing page on 2026-08-30; the monthly figures are
**computed from those rates**, not quoted, because almost nobody publishes a worked example.

| | monthly, 160 h | what drives it |
|---|---:|---|
| **sbx** | **$0** | your own machine; 0 B while asleep |
| zeropod · Sablier · Lazytainer · KubeElasti | **$0** | Apache-2.0 / AGPL-3.0 / MIT / MIT |
| Coder Community | **$0** + your infra | Premium is *contact-us*, no figure published |
| Northflank | ~$12 | $0.01667/vCPU-h + $0.00833/GB-h |
| GitHub Codespaces (Pro) | ~$13 | 180 core-h/month free, then $0.09/core-h |
| Daytona | ~$27 | $200 credit covers roughly the first 1,200 h |
| GitHub Codespaces (org) | ~$31 | **organisations get no free quota at all** |
| Modal | ~$31 | after the $30/month Starter credit |
| Vercel Sandbox | ~$34 | plus $0.08/GB-month for snapshots while stopped |
| Cloudflare Containers | ~$39 | $5 base; CPU billed on *active* use since Nov 2025 |
| Ona (was Gitpod) | ~$40 | *approximate* — OCU rate is published only for a 4 vCPU box |
| Replit Reserved VM | $50 flat | **no sleep discount — the same at 8 h/day or 24/7** |
| E2B | ~$177 | usage is only ~$27; Pro at $150/month is required for sessions over an hour |
| Google Cloud Workstations | ~$183 | **$146 of that is control plane, billed whether or not anything runs** |

Two rows are worth reading twice.

**Google Cloud Workstations bills $0.20/hour per cluster continuously** — $146 a month before a
single workstation starts. That is a fixed charge with no sleep discount, on a product sold for
exactly this job. **Replit's Reserved VM** is the same shape more honestly named: $50 flat,
identical whether you use it one hour a day or all of them.

**Cloudflare moved toward this model rather than away from it.** Since 21 November 2025 its
container CPU is billed on active use rather than provisioned time, which is the same bet sbx
makes — an idle sandbox should cost nothing. Memory and disk are still billed on allocation, so a
sleeping container is cheap there rather than free.

**What the money buys, and this page should say it plainly:** somebody else's machine, somebody
else running it, and an editor that already works. sbx is free because it is your laptop and your
cluster — that is the whole trade, and for a team that already owns both, the hosted bill is for
convenience rather than capability. For a team that owns neither, it is not.

Computed rather than quoted, and worth checking before anyone acts on it: Ona's rate for this
size, and the VM and disk portion of the Workstations figure. Coder Premium, E2B Enterprise and
Daytona Enterprise publish no price at all.
## Dev environments, and the editor story sbx half has

This page had nothing about editors, which is a gap in it rather than in the field: the
dev-environment products are what most people reach for when they want "a machine per branch",
and sbx overlaps them without competing on the thing they sell.

| | sbx | Codespaces | Coder | Ona (Gitpod) | DevPod | code-server |
|---|:---:|:---:|:---:|:---:|:---:|:---:|
| Sleeps on its own | ● | ● 30 min | ● 1 h | ● 30 min | ● 5–10 min | ◐ opt-in flag |
| Self-hosted | ● | ○ | ● AGPL | ◐ BYOC, SaaS control plane | ● | ● |
| Editor integration | ◐ Remote-SSH | ● | ● | ● | ● | ● |
| Reads `devcontainer.json` | ◐ import, lossy | ◐ partial | ● | ● | ● | n/a |
| Traffic to a port counts as activity | ● | ○ **explicitly not** | ○ | ○ **explicitly not** | ○ | ○ |
| Wakes a stopped environment on a connection | ● | ○ | ○ | ○ | ○ | ○ |
| What "idle" means | bytes through the proxy | keyboard and terminal I/O | an active session | editor or SSH attached | a timer | a heartbeat file |

**The row that matters is the third one, and sbx still only half-wins it.** `sbx ssh` prints the
address and the `code --remote ssh-remote+...` line, so VS Code, Cursor and JetBrains Gateway all
attach over the wake path that already exists — the SSH connection *is* the thing that wakes the
sandbox, which is why it needed no new mechanism. `sbx init --from-devcontainer` translates the
`devcontainer.json` a repo already has, and prints what it dropped on stderr rather than pretending
the translation was complete. Both are behind Preview gates.

What is still missing is the thing those products actually sell: **a browser IDE and a managed
machine to run it on.** If that is what somebody wants, none of the rows below make up for it.

**The row below it is the one nobody else has.** Codespaces and Ona both say in their own docs
that traffic to a forwarded port does *not* count as activity — an environment can be serving
requests and still be judged idle. sbx measures bytes through the proxy, so a sandbox being used
is a sandbox that is awake, whatever is using it.

### Nothing sleeps while an editor is attached — including sbx

Checked across Codespaces, Coder, Ona, DevPod, code-server, Google Cloud Workstations and Replit:
every one treats a live editor connection as the reason **not** to sleep. None of them sleep
underneath an attached editor, and neither does sbx.

That is not an oversight in any of them. VS Code's `PersistentProtocol` sends a keepalive every
**5 seconds** unconditionally, with a 20-second dead-connection timeout, and code-server touches
a heartbeat file every minute on top of that — so an attached editor is, by construction, never
idle. Measured here against code-server with a browser tab open and nobody typing: **927–956 B/s
sustained**, against **3136 B/s** while typing. Those are 3.3× apart, and *reading code on screen
produces the idle rate* — so no byte-rate threshold separates "someone is working" from "a tab is
open", and one that slept an abandoned tab would sleep somebody mid-thought.

The technique that would solve it properly does not transfer either. zeropod checkpoints with
CRIU and **dropped** `--tcp-established` in 2025 in favour of skipping in-flight connections,
because CRIU cannot restore a TCP socket across a network hop — which is exactly what an editor
session is. Its docs warn off long-lived connections outright.

So "sleep while the editor stays attached" is unclaimed by everyone, for a reason, and this page
does not claim it either.

### What integration would actually look like

The mechanism decides this, not preference. Of VS Code's three remote modes:

- **Remote-SSH** is a plain inbound TCP dial, so it wakes a sandbox through the mechanism sbx
  already has, with nothing new built. `code --remote ssh-remote+host /path` is documented.
- **Attach to Container** goes through the **Docker socket**, not the container's network — so it
  never opens a connection sbx could wake on. An earlier draft of this work proposed it as the
  integration; the mechanism says otherwise.
- **Remote-Tunnels** needs a `code tunnel` process already running inside, holding an outbound
  connection. There is nothing to intercept.

Which leaves two honest options: an SSH service in the spec, which works today, and reading
`devcontainer.json` so an existing repo needs no second file. DevPod is the closest precedent for
the second — a backend-agnostic client built on the same spec.
## Same spec, two runtimes — and not the same capabilities

"Same spec local + cluster" is a ● doing a lot of work. The spec really is one file and the
everyday commands really are the same — but the backends are not equal. Locally sbx speaks the
Docker Engine API, so a Docker-compatible runtime is a prerequisite (Docker Desktop, Colima,
Rancher Desktop or rootless podman, discovered in that order); the cluster path replaces that
runtime with Kubernetes and the daemon with [`deploy/activator.yaml`](../deploy/activator.yaml).

**On macOS or Windows that runtime is a Linux VM, and nothing here can change it** — a Linux
container is a set of Linux kernel features, so running one on a non-Linux kernel means running a
Linux kernel somewhere, which every one of these products also does. It's a property of
containers, not a gap in sbx; on Linux there's no VM at all. Every row below was checked against
the code. Where a capability is missing it's refused with a reason naming the backend, never
approximated.

| | docker | k8s | |
|---|:---:|:---:|---|
| **Wakes on any TCP connection** | ● | ● | the daemon holds the port locally; the activator does it in-cluster |
| **Sleeps to 0 B when idle** | ● | ● | a stopped container, or a Deployment scaled to zero |
| **Holds the first connection** | ● | ● | waits rather than refusing, on both |
| `list` `env` `logs` `exec` `cp` `rm` · `ready` | ● | ● | the everyday commands |
| **One committed `sandbox.json`** · templates · `depends_on`/`${VAR}` | ● | ● | resolved before either backend sees the spec |
| **cpu / memory limits** | ● | ◐ | docker adjusts in place; a cluster patches the Deployment, **rolling the pod**, and needs rights to patch Deployments — the activator's Role grants `deployments/scale` and nothing more, by design. Works from your kubeconfig; denied to the in-cluster component |
| **removing a limit once set** | ○ | ● | docker's update API reads a zero as "leave unchanged", so a container keeps its ceiling until recreated. The one row the cluster wins outright |
| **cpu / memory usage** | ● | ○ | needs metrics-server in a cluster (the operator's decision); rows read `n/a` rather than imply a sample is coming |
| **Live dashboard** (`sbx ui`) | ● | ◐ | everything but the usage columns and traces, for the row above |
| **Snapshot & fork** | ● | ○ | a cluster's answer is a CSI volume snapshot, not `docker commit` in a hat |
| **`build:`** · **`prewarm`** · **`gc`** | ● | ○ | no local builder or image store in a cluster |
| **`egress: "deny"`** | ● | ○ | the cluster answer is a NetworkPolicy needing an enforcing CNI; refused rather than approximated |
| **`--isolation gvisor\|kata`** | ● | ● | a RuntimeClass in a cluster, refused wherever the runtime is absent |
| **History and audit** | ● | ● | reads a file, so it works when the backend doesn't |
| **`gpus:`** · **`sbx url`** · **metrics** ([`console/`](../console/)) | ● | ○ | a device plugin, an Ingress, and a component that would export from the activator |

## Sources

Vendor docs, read August 2026. Figures attributed to a vendor are on their page, in their words,
and **not reproduced here** — cross-platform latency on different hardware, regions and images is
not like-for-like. Ours are in [BENCHMARKS.md](BENCHMARKS.md) with the machine and script beside
them. Third-party roundup figures are no longer quoted here at all.

- [E2B — persistence](https://docs.e2b.dev/sandbox/persistence): `sandbox.connect()` to resume;
  ~4 s/GiB to pause, ~1 s to resume; filesystem *and* memory preserved.
- [E2B — fork](https://docs.e2b.dev/sandbox/fork): files, processes and memory; no latency from
  idle published; default timeout 5 min, `onTimeout` defaults to **kill** — auto-pause opt-in.
- [Fly Proxy autostop/autostart](https://fly.io/docs/reference/fly-proxy-autostop-autostart/) and
  [suspend/resume](https://fly.io/docs/reference/suspend-resume/): the proxy waits for a request
  and starts a stopped or suspended Machine; "never creates or destroys Machines for you."
- [Cloudflare Sandbox SDK](https://developers.cloudflare.com/sandbox/): `sleepAfter` idle sleep;
  [tunnels](https://developers.cloudflare.com/sandbox/api/tunnels/) via `cloudflared`, which
  [replaced `exposePort()`](https://developers.cloudflare.com/changelog/post/2026-06-09-deprecating-sandbox-sdk-features/)
  in 2026 — the same conclusion this repo reached about its own tunnel, independently.
- [Knative — scale to zero](https://knative.dev/docs/serving/autoscaling/scale-to-zero/) and
  [the activator on the path](https://knative.dev/blog/articles/demystifying-activator-on-path/).
- [Neon — connection latency](https://neon.com/docs/connect/connection-latency): a few hundred ms.
- [Vercel Sandbox](https://vercel.com/docs/vercel-sandbox), [Modal](https://modal.com/docs/guide/sandbox),
  [Daytona](https://www.daytona.io/docs/) (advertises a cold start "under 90ms" — no percentile,
  no wake figure), [Northflank](https://northflank.com/blog/preview-environment-platforms).

Self-hosted prior art, read from the repositories:

- [ctrox/zeropod](https://github.com/ctrox/zeropod) — containerd shim, CRIU, eBPF; restore "in
  tens to a few hundred milliseconds"; arm64 in a Linux VM on macOS "somewhat flaky".
- [sablierapp/sablier](https://github.com/sablierapp/sablier) — API server + reverse-proxy
  middleware; five providers; blocking and dynamic strategies; ~1.5–2 ms/request overhead.
- [vmorganp/Lazytainer](https://github.com/vmorganp/Lazytainer) — packet-count thresholds; "apply
  a label and proxy their traffic through the Lazytainer container".
