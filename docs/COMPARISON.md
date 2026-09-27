# sbx vs E2B, Daytona, OpenSandbox, Testcontainers and others

> **Who this is for:** anyone choosing between sbx and another sandbox, hosted or self-hosted, or
> a local tool such as docker compose or Testcontainers. It covers where sbx wins, where it
> loses, and when to pick something else.
> **Vendor cells re-verified 2026-09-27** unless a row carries its own date.
> **Sourcing rule:** every sbx number is measured by a script in this repo
> ([BENCHMARKS.md](BENCHMARKS.md)). Every vendor fact is quoted from the vendor's own page, with a
> link. A cell nobody checked says `–`, never a guess.

sbx is self-hosted sandboxes for every branch and AI agent: a real Postgres, Redis or browser that
sleeps at 0 B of RAM and wakes the moment anything connects. No SDK required, no account, one
binary. Unfamiliar terms (microVM, gVisor, OpenSandbox, egress) are explained in the
[README glossary](../README.md#glossary).

## At a glance

● yes · ◐ partial or conditional · ○ no · – not documented or not checked.
"microVM" means `--provider firecracker`; "docker" is the default provider.

| | sbx | E2B | Daytona | Modal | OpenSandbox | Vercel | Cloudflare | Fly |
|---|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|
| Wakes on an unmodified client connection | ● any TCP | ○ SDK | ○ API | ○ SDK | ○ API | ○ SDK | ◐ HTTP² | ◐ HTTP¹ |
| Runs on your laptop | ● | ○ | ○ | ○ | ● | ○ | ○ | ○ |
| Self-hosted, no account | ● MIT | ◐ infra³ | ◐ frozen⁴ | ○ | ● Apache-2.0 | ○ | ○ | ○ |
| Hosted, nothing to run | ○ | ● | ● | ● | ○ | ● | ● | ● |
| Several services in one spec | ● | ○ | ○ | ○ | – | ○ | ○ | ◐ |
| Idle cost | $0, 0 B RAM | storage | storage | storage | $0, your infra | snapshot storage | storage | storage |
| Sleep keeps RAM + processes | ● microVM · ◐ docker⁵ | ● | ◐ VM only | ◐ alpha | ● | ○ files only | ○ | ● suspend |
| Isolation boundary | container · jailed microVM | Firecracker | container; VM optional | gVisor | container to Firecracker | Firecracker | container in a VM | Firecracker |
| SDKs | OpenSandbox's 5 · CLI · MCP | Python, JS | 5 languages | Python, JS, Go [src][modal-sdk] | 5 languages | TS, Python | TS | REST API |
| Speaks the OpenSandbox API | ● | ○ | ○ | ○ | ● reference | ○ | ○ | – |
| Egress policy, changed live | ◐ docker, microVM | ● | ◐ by tier | ● CIDR; domains beta | ● | ● | ● HTTP | – |
| Credentials kept out of the sandbox | ○ [planned](ROADMAP.md#next) | – | – | – | ● vault | ● brokering | ◐ header injection | – |
| GPU | ◐ docker only | – | – | ● | – | – | ○ | ● |

¹ Through Fly Proxy. HTTP wakes freely; raw TCP needs a dedicated IPv4 and is unreliable on a
shared one (checked 2026-08-31). ² A request through your Worker. ³ Its infrastructure code is
Apache-2.0. ⁴ AGPL; the open-source core has had no updates since 2026-06. ⁵ Opt-in
`on_idle: "freeze"`: memory and processes are kept, so RAM stays in use while idle
([SPEC](SPEC.md#on_idle-freeze-keeps-memory-instead)).

Also worth knowing: microsandbox (self-hosted microVMs, Linux, Mac and Windows) and Docker
Sandboxes (runs coding agents in microVMs) are in [the details](#each-alternative-briefly). For a
laptop or CI, the closer rivals are [docker compose and Testcontainers](#vs-docker-compose-and-testcontainers).
Neon matters only if all you need is Postgres.

## Which one should I use?

| You want | Use | Why |
|---|---|---|
| A database or stack per branch, PR or agent, on your own machine | **sbx** | one spec, assigned ports, 0 B of RAM while asleep |
| Unmodified clients (`psql`, pools, Playwright) to wake a sleeping service | **sbx** | the first connection is held, not refused, over any TCP protocol |
| OpenSandbox or E2B-style agent sandboxes, self-hosted, from one binary | **sbx** | OpenSandbox's SDKs and MCP tools work unchanged |
| One spec on a laptop, a cluster and a microVM | **sbx** | docker, kubernetes and firecracker providers |
| Test containers started and stopped from inside test code | Testcontainers | in-process lifecycle and a large module ecosystem; `sbx with` if you want any language |
| A few always-on services for one project | docker compose | already installed, nothing new to learn |
| Nothing to run yourself | E2B, Daytona, Modal, Vercel or Cloudflare | hosted; sbx has no hosted option |
| A VM boundary on a Mac or Windows, run end to end today | microsandbox | sbx's helper-VM path is **run by hand** on a Mac (v0.11) and **not yet run end to end** on Windows ([status](../README.md#platform-status)) |
| To run a coding agent itself in a microVM | Docker Sandboxes | that is its job; sbx runs the services an agent uses |
| Only Postgres branches | Neon | wakes on the Postgres protocol and branches the data |

### Also choose something else if…

Each tool named here is described in [the details](#each-alternative-briefly).

- **You need the fastest burst create today.** isorun and Daytona lead ComputeSDK's board.
- **You need secrets kept out of the sandbox now.** OpenSandbox's vault, Vercel's brokering or
  isorun's proxy do it; sbx does not yet.
- **You want a browser IDE on a managed machine.** Pick Codespaces, Coder or Ona.
- **Your apps are HTTP and already behind Traefik or Caddy.** Sablier fits that stack.
- **You want memory restore on a Kubernetes cluster you already run.** zeropod does it as a shim.
- **You need GPUs at scale.** Modal or Fly.
- **You need a large community and support.** Every alternative here is bigger.

## Honest ranking

**This is the project's own judgement, not a measurement.** It ranks sbx among the eight tools in
[At a glance](#at-a-glance) plus microsandbox (10 in all, counting sbx). 1 is best. Sources are the
links on this page and [BENCHMARKS.md](BENCHMARKS.md).

| Dimension | sbx rank | Why |
|---|:---:|---|
| Idle cost | **1** (tied with OpenSandbox on $) | 0 B RAM asleep on your own hardware, measured. Hosted rivals still bill storage |
| Multi-service stacks | **1** (OpenSandbox not checked) | one `sandbox.json` with Postgres, Redis and a browser; the rest model one box you exec into |
| Self-hosting | **2** | one binary, laptop to cluster; OpenSandbox has more runtimes and a far larger community |
| Docs (depth, honesty) | 4 | measured numbers with scripts; no docs site, search or cookbook |
| SDK breadth | 4 | OpenSandbox's 5 SDKs work unchanged, but its isolated sessions and vault are not built |
| Onboarding | 5 | install, `sbx serve`, create; needs a Docker runtime. Vercel and Docker are one command |
| Isolation | 6 (tied) | the default is a container, as on OpenSandbox; the jailed microVM is opt-in |
| Community and adoption | **10 (last)** | 2 GitHub stars on 2026-09-27; OpenSandbox 15.5k, E2B 14k, microsandbox 8.4k |
| Hosted option | **10 (last, tied)** | none, by design; OpenSandbox and microsandbox have none either |

**Not ranked**, because no like-for-like measurement exists:

- **Wake latency (asleep → serving).** sbx: 216 ms Redis and 348 ms Postgres on docker (v0.14.0,
  Linux cloud VM); 216 ms for a microVM on an M4 through the helper VM (v0.11.0, before the
  jailer). Vendors publish resume times measured their own way ([below](#what-asleep-costs-and-what-it-keeps)).
- **Create → first command.** sbx is not on ComputeSDK's board
  ([below](#create--first-command-measured-the-same-way)).

## vs docker compose and Testcontainers

For branch environments and CI fixtures, these are what most teams use today.

- **docker compose** is Docker's tool for defining and running multi-container applications
  ([docs][compose]). Services run until you stop them, and a fixed host port such as `5432:5432`
  clashes when two branches want it; leave the host port out and Docker picks one ([ports][compose-ports]).
- **Testcontainers** is a family of open-source libraries (Java, Go, .NET, Node.js, Python, Rust
  and more) that start "throwaway, lightweight instances of databases, message brokers, web
  browsers" from inside your tests ([site][tc]). Each container gets a random free host port
  ([networking][tc-net]).

| | sbx | docker compose | Testcontainers |
|---|:---:|:---:|:---:|
| Ports | ● assigned, read from `sbx env` | ◐ fixed unless you omit the host port | ● random free port |
| Idle services | ● sleep at 0 B of RAM, wake on connect | ○ run until stopped | ○ removed after the tests |
| Lives across test runs and branches | ● | ● | ◐ per test session by default |
| Snapshot a seeded database, fork it per branch | ● `sbx snapshot`, `sbx fork` | ○ | ○ |
| Language | ● any, through environment variables | ● any | ◐ a library per language |
| Control from inside test code | ◐ `sbx with -- <test command>` | ○ | ● in-process lifecycle |
| Ready-made modules for common services | ◐ 5 templates | ○ | ● a large module catalogue |

**Testcontainers wins** when the test code should own the container: in-process lifecycle, typed
modules, and a large ecosystem. **Compose wins** when a few services always run for one project.
**sbx wins** when many copies of a stack must coexist on one machine, sleep when unused, and be
reached by tools that know nothing about sbx. Moving a compose file: [SPEC](SPEC.md#coming-from-docker-compose).

## The axis that actually separates them

Everybody scales to zero. The question is **what has to happen to bring it back**, because that
decides who is allowed to be the client.

| | wakes on | so the client can be | runs off-cloud |
|---|---|---|---|
| **sbx** | **any TCP connection**, held until ready | anything with a socket | **laptop + cluster** |
| E2B | `sandbox.connect()` | only your own code | ◐ self-host infra |
| Daytona · Modal | an API or SDK call | only your own code | ○ |
| Vercel Sandbox | any SDK call auto-resumes | only your own code | ○ |
| Cloudflare Sandbox | a request through your Worker | HTTP to your Worker | ○ |
| Fly Machines | a request through Fly Proxy | HTTP freely; raw TCP needs a dedicated IPv4 | ○ their proxy |
| Knative (serverless on Kubernetes) | an HTTP request through its activator | HTTP, gRPC, WebSocket | ● needs a cluster |
| Neon | a Postgres connection | Postgres clients only | ○ |

A connection pool cannot call `sandbox.connect()`. Neither can `psql`, `pg_dump`, a migration tool
or Playwright over CDP. On an SDK-woken platform, something in your code must wake the sandbox
first. Here the socket is the signal, and the tools stay unmodified.

Fly Proxy and Neon get this right, and the idea is not novel against them. Both are hosted. sbx
runs on your laptop, with no account, for any protocol. Sources: [Vercel persistence][vc-p],
[Cloudflare lifecycle][cf-sb], [Fly Proxy][fly-proxy], [Knative][knative], [Neon latency][neon-lat].

## What "asleep" costs, and what it keeps

| | at rest | wake | what survives |
|---|---|---|---|
| **sbx, docker** | **0 B RAM**, plus its volume | **216 ms** Redis · 348 ms Postgres · 1534 ms k8s | disk; processes cold-start |
| **sbx, microVM** | 0 B RAM; a memory image on disk | 216 ms to first byte, M4 through the helper VM (v0.11.0) | **RAM + running processes** |
| E2B | storage | ~1 s resume [src][e2b-p] | disk, RAM, processes |
| Fly, suspended · stopped | storage | a few hundred ms · ~2 s+ [src][fly-sr] | RAM · disk |
| Cloudflare | – | 1–3 s from stopped [src][cf-arch] | nothing: files deleted, processes end [src][cf-sb] |
| Vercel | snapshot storage | – | filesystem [src][vc-p] |
| Neon | storage | a few hundred ms [src][neon-lat] | Postgres data |

sbx figures come from `scripts/bench.sh` and the helper-VM run in [BENCHMARKS.md](BENCHMARKS.md),
where each carries its machine and date. Redis and Postgres are v0.14.0 on a Linux cloud VM.
The 1534 ms k8s figure is v0.1.0 and has not been re-run; the microVM figure predates the jailer.

A microVM sleep is a snapshot, so a woken Postgres resumes rather than replaying its WAL. The cost
is disk: the memory image is roughly the VM's RAM. On docker, `on_idle: "freeze"` keeps memory by pausing
instead of sleeping, which holds the RAM; `sbx checkpoint` and `sbx resume` save memory to disk
through CRIU (a Linux tool that saves a running process and restores it) on a podman runtime only.

## Create → first command, measured the same way

A vendor's "create" figure is whatever that vendor chose to time. The one comparable number is
**ComputeSDK's Burst TTI**: 100 concurrent creates, each timed to a successful `node -v`, run daily
from one runner ([methodology][csdk-m]). Run of 2026-09-25 ([results][csdk-r]):

| | median TTI | | median TTI |
|---|---:|---|---:|
| isorun (#1) | 43.6 ms | Vercel | 452.9 ms |
| Daytona | 341.4 ms | Cloudflare | 647.8 ms |
| Modal | 907.6 ms | E2B | 1,237.7 ms |

**sbx and OpenSandbox are not on it.** The board benchmarks hosted services. sbx's own scripts run
the same shape, which is not a leaderboard entry:

| sbx, same shape | median | where |
|---|---:|---|
| docker, warm pool, 1 at a time | 12.8 ms | Linux cloud VM, 4 vCPU (v0.14.0); 13.7 ms on Apple M4, colima |
| docker, warm pool, 100 at once | 309-460 ms | Linux cloud VM, 4 vCPU (v0.14.0); 472.1 ms on Apple M4, colima |
| microVM, frozen pool, 4 at once | 144 ms | GitHub runner, nested KVM (v0.14.0); 141 ms at v0.13 |
| microVM, cold, 4 at once | 2,637 ms | GitHub runner, nested KVM (v0.14.0); 2,821 ms at v0.13 |

Different machines, concurrency and no network hop: read these as what sbx does on that hardware.
Each figure's script, run and older values are in [BENCHMARKS.md](BENCHMARKS.md); that page is
the source if a number here ever disagrees with it.
There is no bare-metal microVM number yet ([ROADMAP](ROADMAP.md#now)).

## Each alternative, briefly

**E2B** — a hosted service that runs AI agents' code in Firecracker microVMs, with Python and JS SDKs. Pause keeps memory and processes,
~1 s to resume ([persistence][e2b-p]). The default timeout is 5 min and `onTimeout` defaults to
kill; auto-pause is opt-in ([fork][e2b-fork]). Its infrastructure is Apache-2.0 ([e2b-dev/infra][e2b-infra]).

**Daytona** — a hosted service for AI-agent sandboxes, with 5 SDKs; it advertises creation "sub 90ms" ([pricing][dt-price]). Memory
is kept only for VM sandboxes ([sandboxes][dt-sb]). The open-source repo says core development
moved to a private codebase in June 2026 and it "will receive no further updates" ([repo][dt-gh]).

**Modal** — a hosted serverless platform for Python and AI workloads with sandboxes; gVisor isolation ([security][modal-sec]); memory snapshots are alpha
([snapshots][modal-snap]). Egress by CIDR, domains in beta ([networking][modal-net]).

**OpenSandbox** — an open-source API standard for AI-agent sandboxes, with SDKs in 5 languages; this is its reference server, whose API sbx implements. Self-hosted, Apache-2.0,
container, gVisor, Kata or Firecracker runtimes, a credential vault, and "~80ms" Firecracker
pools ([repo][osb], [performance][osb-perf]). sbx serves its SDKs from one binary; its isolated
sessions and vault are [not built in sbx yet](ROADMAP.md#not-built-yet-in-the-opensandbox-api).

**Vercel Sandbox** — Vercel's hosted sandboxes for running untrusted code, on Firecracker. Persistence is the default, but it saves the filesystem,
not memory; any SDK call auto-resumes a stopped sandbox ([persistence][vc-p]). Egress firewall and
credential brokering ([firewall][vc-fw]).

**Cloudflare Sandbox** — containers you drive from a Cloudflare Worker (their serverless functions). It sleeps after 10 min by default (`sleepAfter`); on sleep, files are deleted
and processes end, and the next request starts a fresh container ([lifecycle][cf-sb]). Persistence
is an R2 backup ([backup][cf-bk]). Has a code interpreter and header injection ([docs][cf]).

**Fly Machines and Sprites** — Fly.io's hosted VMs. Fly Proxy stops or suspends idle Machines and starts them on the
next request ([autostop][fly-proxy], [suspend][fly-sr]). Sprites wake on inbound HTTP ([sprites][fly-sprites]).

**microsandbox** — an open-source tool that runs sandboxes as self-hosted microVMs on Linux, macOS and Windows; forks live sandboxes; SDKs in
5 languages; no daemon; "beta software" ([repo][msb]). Closest to sbx's microVM provider, but it
does not document waking on a connection or a multi-service spec.

**isorun** — a hosted sandbox service on KVM virtual machines, #1 on the Burst TTI board; a pause keeps the same PIDs
([lifecycle][iso-l]); a credential proxy ([site][iso]).

### Docker Sandboxes (the other `sbx`)

Docker's product installs a CLI also named `sbx` (`brew install docker/tap/sbx`) and runs coding
agents such as Claude Code and Codex in microVMs, locally or in Docker's cloud ([product][docker-sb]).
It runs the agent. This sbx runs the services an agent or a branch needs. If both are installed,
the one first on `PATH` wins.

**Neon** — a hosted Postgres service with copy-on-write branches that wakes on a Postgres connection, "a few
hundred milliseconds" ([latency][neon-lat]). If Postgres is all you need, it is the better fit.

## The self-hosted prior art

The projects that solve the same wake problem on your own machines. Read them before this one.

| | wakes on any TCP | no runtime to replace | laptop-first | per-service spec | restores RAM |
|---|---|---|---|---|---|
| **sbx** | ● held, not refused | ● | ● | ● | ◐ microVM |
| [zeropod][zeropod] (952★) | ● | ○ shim + CRIU + eBPF | ○ | ○ | ● |
| [Lazytainer][lazy] | ● | ◐ owns networking | ● | ○ | ○ |
| [Sablier][sablier] | ○ HTTP only (v1.17.0) | ● | ● | ◐ labels | ○ |
| [KubeElasti][elasti] | ○ HTTP/gRPC, not verified | ● | ○ | ○ | ○ |

- **zeropod** checkpoints a container with CRIU after idle and restores it on the first
  connection. Measured against this repo: **272 ms median, n=4, 4/4 first attempts served**
  (`scripts/zeropod-probe.sh`). It needs a cluster, and calls arm64 in a Linux VM on macOS
  "somewhat flaky".
- **Sablier** is an API that reverse-proxy middleware calls (Traefik, Caddy, Nginx and others). It
  is HTTP-only, so nothing wakes `psql`, with ~1.5–2 ms per request against sbx's ~10 µs per round trip (v0.14.0). An
  unofficial [sablier-proxy][sablier-proxy] adds TCP (checked 2026-08-30).
- **Lazytainer** stops containers below a packet threshold, and your traffic must route through
  it. Side by side, **sbx served 20/20 first connections; Lazytainer served 0/5** (v0.14.0, BENCHMARKS.md).
- **KubeElasti** queues requests while a Deployment is at zero; Kubernetes only.

What none of them combines: arbitrary TCP, no runtime to replace, a committed spec file, and the
same binary on a laptop and a cluster.

## What the alternatives cost, for one developer

<details>
<summary>Monthly cost of each alternative</summary>

One environment of about 2 vCPU and 4 GB, 8 h a day, 20 days a month (160 h). Rates read from each
vendor's pricing page on **2026-08-30**; the monthly figures are **computed**, not quoted.

| | monthly, 160 h | what drives it |
|---|---:|---|
| **sbx** | **$0** | your own machine; 0 B RAM while asleep |
| zeropod · Sablier · Lazytainer · KubeElasti | $0 | open source |
| Coder Community | $0 + your infra | Premium publishes no price |
| Northflank | ~$12 | $0.01667/vCPU-h + $0.00833/GB-h |
| GitHub Codespaces (Pro) | ~$13 | 180 core-h/month free, then $0.09/core-h |
| Daytona | ~$27 | $200 credit covers roughly the first 1,200 h |
| GitHub Codespaces (org) | ~$31 | organisations get no free quota |
| Modal | ~$31 | after the $30/month Starter credit |
| Vercel Sandbox | ~$34 | plus $0.08/GB-month for snapshots while stopped |
| Cloudflare Containers | ~$39 | $5 base; CPU billed on active use since Nov 2025 |
| Ona (was Gitpod) | ~$40 | approximate: rate published only for a 4 vCPU box |
| Replit Reserved VM | $50 flat | no sleep discount |
| E2B | ~$177 | usage ~$27; Pro at $150/month needed for sessions over 1 h |
| Google Cloud Workstations | ~$183 | $146 is the control plane, billed even when idle |

What the money buys: somebody else's machine, somebody else running it, and an editor that already
works. sbx is free because it is your hardware. For a team that owns none, that is not a saving.

</details>

## Dev environments

sbx is not a dev-environment product, but it is often compared with one. The short answer: pick
Codespaces, Coder or Ona for an editor on a managed machine.

<details>
<summary>Dev environments, compared</summary>

| | sbx | Codespaces | Coder | Ona | DevPod | code-server |
|---|:---:|:---:|:---:|:---:|:---:|:---:|
| Sleeps on its own | ● | ● 30 min | ● 1 h | ● 30 min | ● 5–10 min | ◐ opt-in |
| Self-hosted | ● | ○ | ● AGPL | ◐ BYOC | ● | ● |
| Editor integration | ◐ Remote-SSH | ● | ● | ● | ● | ● |
| Reads `devcontainer.json` | ◐ import, lossy | ◐ | ● | ● | ● | n/a |
| Traffic to a port counts as activity | ● | ○ | ○ | ○ | ○ | ○ |
| Wakes on a connection | ● | ○ | ○ | ○ | ○ | ○ |

`sbx ssh` lets VS Code, Cursor and JetBrains Gateway attach over the wake path; `sbx init
--from-devcontainer` imports a `devcontainer.json` and prints what it dropped. Both are Preview
gates. There is no browser IDE and none is planned.

Nothing here sleeps while an editor is attached, sbx included. VS Code sends a keepalive every
5 s; measured against code-server with an idle tab, **927–956 B/s** against 3136 B/s while typing.
No byte threshold separates "reading code" from "tab left open". Of VS Code's remote modes, only
Remote-SSH opens a TCP connection sbx can wake on.

</details>

## One spec, three providers

The spec and everyday commands are the same on docker, kubernetes and microVM; capabilities are
not. All three wake on any TCP connection and hold the first one (kubernetes through an activator,
a proxy that holds it while the pod scales up), and all three sleep to 0 B of RAM. Only the
microVM keeps memory and processes while asleep. Which fields each provider accepts or refuses is
in [SPEC](SPEC.md#provider-support); how each one works is in [ARCHITECTURE.md](ARCHITECTURE.md).

## Corrections

Vendor cells this page got wrong, and what changed. Commits carry the detail.

<details>
<summary>Corrections log</summary>

| date | was | now |
|---|---|---|
| 2026-08-15 | sbx daemon 4.5 MB | 9.1 MB, measured |
| 2026-08-16 | Neon "300–800 ms"; E2B fork "5–30 ms" | Neon says "a few hundred ms"; E2B publishes no fork figure |
| 2026-08-16 | E2B fan-out "tens of ms" | E2B publishes ~1 s resume, no fan-out figure |
| 2026-08-16 | Daytona "~90 ms p99" wake | a cold start "under 90ms", no percentile, no wake figure |
| 2026-08-16 | E2B scales to zero after 5 min | the timeout kills by default; auto-pause is opt-in |
| 2026-08-31 | Fly wakes on a raw socket ● | ◐: raw TCP needs a dedicated IPv4 |
| 2026-08-31 | Daytona self-hosted ● | ◐: open-source core frozen since 2026-06 |
| 2026-09-27 | Cloudflare, Modal, Daytona, E2B cells | RAM-state and isolation cells corrected to the vendors' pages |
| 2026-09-27 | sbx restores RAM only via podman; isolation via gVisor/Kata | microVM sleep keeps RAM; jailed microVM provider |
| 2026-09-27 | OpenSandbox at `alibaba/OpenSandbox` | `opensandbox-group/OpenSandbox`; zeropod at `laravel/zeropod` |
| 2026-09-27 | docker sleep keeps RAM ○; wake latency ranked 1–2 | ◐ via `on_idle: "freeze"`; wake latency not ranked (no like-for-like data) |

</details>

[cf]: https://developers.cloudflare.com/sandbox/
[cf-sb]: https://developers.cloudflare.com/sandbox/concepts/sandboxes/
[cf-bk]: https://developers.cloudflare.com/sandbox/guides/backup-restore/
[cf-arch]: https://developers.cloudflare.com/containers/platform-details/architecture/
[compose]: https://docs.docker.com/compose/
[compose-ports]: https://docs.docker.com/reference/compose-file/services/#ports
[csdk-m]: https://github.com/computesdk/benchmarks/blob/master/METHODOLOGY.md
[csdk-r]: https://github.com/computesdk/benchmarks/blob/master/results/burst_tti/latest.json
[docker-sb]: https://www.docker.com/products/docker-sandboxes/
[dt-gh]: https://github.com/daytonaio/daytona
[dt-price]: https://www.daytona.io/pricing
[dt-sb]: https://www.daytona.io/docs/en/sandboxes
[e2b-fork]: https://docs.e2b.dev/sandbox/fork
[e2b-infra]: https://github.com/e2b-dev/infra
[e2b-p]: https://docs.e2b.dev/sandbox/persistence
[elasti]: https://github.com/truefoundry/KubeElasti
[fly-proxy]: https://fly.io/docs/reference/fly-proxy-autostop-autostart/
[fly-sprites]: https://fly.io/sprites/
[fly-sr]: https://fly.io/docs/reference/suspend-resume/
[iso]: https://isorun.ai/
[iso-l]: https://docs.isorun.ai/sandboxes/lifecycle
[knative]: https://knative.dev/docs/serving/autoscaling/scale-to-zero/
[lazy]: https://github.com/vmorganp/Lazytainer
[modal-net]: https://modal.com/docs/guide/sandbox-networking
[modal-sdk]: https://modal.com/docs/guide/sdk-javascript-go
[modal-sec]: https://modal.com/docs/guide/security
[modal-snap]: https://modal.com/docs/guide/sandbox-snapshots
[msb]: https://github.com/superradcompany/microsandbox
[neon-lat]: https://neon.com/docs/connect/connection-latency
[osb]: https://github.com/opensandbox-group/OpenSandbox
[osb-perf]: https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/architecture/fast-sandbox/performance.md
[sablier]: https://github.com/sablierapp/sablier
[tc]: https://testcontainers.com/
[tc-net]: https://java.testcontainers.org/features/networking/
[sablier-proxy]: https://github.com/vbrandl/sablier-proxy
[vc-fw]: https://vercel.com/docs/sandbox/concepts/firewall
[vc-p]: https://vercel.com/docs/sandbox/concepts/persistent-sandboxes
[zeropod]: https://github.com/laravel/zeropod
