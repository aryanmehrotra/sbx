# Compared

Every figure here is either measured by a script in this repo or quoted from the vendor's own
documentation, with a link. Nothing is quoted from a competitor's marketing page about a
competitor.

---

## First, two different things are called "sandbox"

Most of the platforms people compare this to are not doing the same job. The word covers two
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

**sbx is the right-hand column.** If you are running untrusted model-authored code and need a
kernel boundary, the left-hand column is what you want, and this is not competing for that —
see [where to use something else](#where-to-use-something-else).

---

## The axis that actually separates them

Everybody scales to zero. The question nobody's table asks is **what has to happen to bring it
back**, because that determines who is allowed to be the client.

| | wakes on | so the client can be | runs off-cloud |
|---|---|---|---|
| **sbx** | **any TCP connection** | anything with a socket | **laptop + cluster** |
| E2B | `sandbox.connect()` — an SDK call | only your own code | ✗ hosted |
| Daytona | start/resume API call | only your own code | ✗ hosted |
| Modal | SDK call | only your own code | ✗ hosted |
| Vercel Sandbox | SDK call | only your own code | ✗ hosted |
| Cloudflare Sandbox SDK | SDK call over RPC | only your own code | ✗ hosted |
| Fly Machines | a request through Fly Proxy | anything, incl. TCP services | ✗ their proxy |
| Knative | an HTTP request through the activator | HTTP/gRPC/WebSocket only | ✓ needs a cluster |
| Neon | a Postgres connection | Postgres clients only | ✗ hosted |

**Why this is the whole point.** A connection pool cannot call `sandbox.connect()`. Neither can
`psql`, `pg_dump`, a migration tool, Playwright over CDP, or a test runner someone else wrote.
On any SDK-triggered platform, *something in your code* has to know the sandbox is asleep and
wake it first — which means every tool that touches it must be sandbox-aware.

Here, the socket is the signal. The tools stay unmodified and don't know they're talking to
something that was at 0 B a moment ago.

Two platforms get this right and are worth naming honestly: **Fly Proxy** wakes on a real
connection and supports TCP services, and **Neon** wakes on the Postgres protocol itself. Both
are hosted, both are excellent, and the ideas here are not novel against them — what's
different is that this runs on your laptop, with no account, on any protocol.

---

## What "asleep" costs, and what it keeps

| | at rest | wake | what survives |
|---|---|---|---|
| **sbx** | **0 B** | **191 ms** docker · 1534 ms k8s | disk. processes cold-start |
| E2B | storage only | **~1 s** | **disk + RAM + running processes** |
| Daytona | storage only | ~90 ms p99 reported | disk, persistent volume |
| Fly (suspended) | storage only | a few hundred ms | RAM snapshot |
| Fly (stopped) | storage only | ~2 s+ | disk |
| Neon | storage only | 300–800 ms | Postgres data |
| Knative | 0 pods | pod schedule, seconds | volume, if you attached one |

**The honest weakness, stated plainly.** E2B pauses to a memory snapshot: loaded variables and
running processes come back exactly as they were, at roughly 4 s per GiB to pause and ~1 s to
resume. sbx does not snapshot RAM. A sleeping sandbox is a stopped container with its volume
intact, so a wake is a **cold process start against warm data** — Postgres replays its WAL and
comes up, it doesn't resume mid-transaction.

That is a worse guarantee. It is also why a sleeping sandbox is genuinely **0 B** rather than
"storage only", and why sleeping costs nothing to enter — there is no 4 s/GiB checkpoint to pay
before the saving starts. For a database on a branch, disk-warm is the state that matters. For
an agent's half-finished Python REPL, it isn't, and E2B is the better tool.

---

## Where this sits

Two charts, because one of them flatters us and the other one doesn't, and only showing the
first would be the kind of comparison this file exists to avoid.

### 1 · Speed against breadth — the chart everyone draws

```mermaid
quadrantChart
    title Wake latency against platform breadth
    x-axis Slower to wake --> Faster to wake
    y-axis Narrow platform --> Broad platform
    quadrant-1 fast and broad
    quadrant-2 broad but slower
    quadrant-3 narrow and slow
    quadrant-4 fast and narrow
    sbx docker: [0.76, 0.25]
    sbx k8s: [0.31, 0.32]
    E2B: [0.40, 0.80]
    Daytona: [0.90, 0.74]
    Fly suspended: [0.68, 0.90]
    Fly stopped: [0.22, 0.86]
    Neon: [0.55, 0.58]
    Knative: [0.09, 0.42]
```

**We are in the bottom-right, and that is the correct place for us to be.** Fast, and narrow.
Anything claiming to be top-right on its own comparison page has chosen its own axes.

The breadth score deliberately excludes everything sbx is good at. It counts eight things we
mostly don't do — VM-grade isolation, RAM snapshotting, GPU, arbitrary stateful services, a
public URL, somebody else operating it, multi-tenant security, production-proven — at 1 for
yes and ½ for partial. Scored on the rows *we* would have picked, every one of these charts
would put us top-right, which is exactly why the score doesn't use them.

| | wake, ms | source | breadth | of 8 |
|---|---|---|---|---|
| Daytona | ~90 *reported* | third-party roundup | 0.81 | 6.5 |
| **sbx** docker | **191** | `scripts/bench.sh 20`, this repo | **0.25** | **2** |
| Fly, suspended | a few hundred | [vendor][fly] | 0.95 | 7.5 |
| Neon | 300–800 | [vendor][neon] | 0.60 | 4.8 |
| E2B | ~1000 | [vendor][e2b] | 0.88 | 7 |
| **sbx** kubernetes | **1534** | `scripts/bench.sh`, minikube | **0.30** | 2.5 |
| Fly, stopped | ~2000+ | [vendor][fly] | 0.95 | 7.5 |
| Knative | seconds, pod schedule | — | 0.50 | 4 |

Point positions are those scores, rounded a little so the labels don't sit on top of each
other; the table is the data, the chart is the shape.

⚠️ **The x-axis is not a fair race and is on a log scale.** Ours is loopback on an idle laptop;
theirs is a multi-tenant fleet across a network, and E2B's second restores a RAM image while
our 191 ms starts a process. [BENCHMARKS.md](BENCHMARKS.md#against-other-platforms) lists all
four reasons, and they all favour us. Modal and Cloudflare are absent because we could not
find a vendor-documented wake figure for either — a blank is better than a guess.

### 2 · The chart that explains why this exists

Same platforms, the two axes that are structural rather than measured — so no hardware,
network or region can flatter anybody.

```mermaid
quadrantChart
    title What can wake it, and where it can run
    x-axis Only your own code --> Any client with a socket
    y-axis Someone elses cloud --> Your machine and your cluster
    quadrant-1 any client, your hardware
    quadrant-2 your hardware, narrow trigger
    quadrant-3 hosted, SDK-triggered
    quadrant-4 hosted, open trigger
    sbx: [0.92, 0.80]
    zeropod: [0.88, 0.62]
    Lazytainer: [0.72, 0.90]
    Sablier: [0.38, 0.88]
    Knative: [0.45, 0.72]
    Daytona: [0.12, 0.40]
    Fly Machines: [0.78, 0.15]
    Neon: [0.50, 0.10]
    Modal: [0.10, 0.23]
    Cloudflare: [0.16, 0.17]
    Vercel Sandbox: [0.13, 0.11]
    E2B: [0.09, 0.05]
```

**The hosted platforms are all in the bottom-left, and the top-right is not ours alone.** Fly
is far right because its proxy wakes on a real connection, but it's their proxy in their
cloud. Knative is high because you run it yourself, but it only wakes on HTTP. Neon sits
mid-x because the Postgres wire protocol *is* the trigger — the right idea, one protocol wide.

The three neighbours in that corner are the ones worth reading before this one, and they are
covered next.

---

## The closest prior art

Not the hosted platforms — three self-hosted Go projects that solve the same problem, and the
ones to read before this one. Two of them predate it.

### zeropod · [ctrox/zeropod](https://github.com/ctrox/zeropod) · 939★

A containerd shim. eBPF watches TCP activity; after an idle period CRIU checkpoints the
container to disk; an activator holds the port and **restores on the first connection in tens
to a few hundred milliseconds** — with memory, open files and processes intact.

**This is the same mechanism as ours, done at a lower layer, and on the RAM question it beats
us outright.** What we restore is a disk. What it restores is the process.

Where it costs more: it replaces your container runtime. It needs a containerd shim installed
on every node, CRIU, eBPF and a Kubernetes cluster — root-level infrastructure, configured per
node. Its README calls arm64 workloads in a Linux VM on macOS "somewhat flaky", which is
precisely Docker Desktop on an Apple Silicon laptop. It is a cluster technology; this is a
userspace binary you run as yourself.

### Sablier · [sablierapp/sablier](https://github.com/sablierapp/sablier) · 2,888★

The most popular of the three, and the most different. An API server that reverse proxies
(Traefik, Caddy, Nginx, Envoy, APISIX, Istio) call through middleware, with five providers:
docker, swarm, podman, kubernetes, Proxmox LXC. Blocking strategy holds the request; dynamic
strategy shows a themed waiting page.

**It is HTTP-only.** The wake is a middleware hook on an HTTP request, so there is no path by
which `psql` wakes anything — you would need a reverse proxy that speaks the Postgres wire
protocol, which is the problem it declines to solve. Steady-state overhead is ~1.5–2 ms per
request against our ~15 µs, because a proxy that parses is not a proxy that splices.

Worth stealing: the waiting page. A human who clicks a link and sees a themed "starting…"
page has a better time than one who watches a spinner, and `sbx url` currently offers the
spinner.

### Lazytainer · [vmorganp/Lazytainer](https://github.com/vmorganp/Lazytainer) · 754★

Counts packets on a network interface and stops containers that fall below a threshold
(default 30 packets). Protocol-agnostic like ours, and the closest in spirit.

Where it costs more: containers must be labelled into a group **and have their traffic routed
through the Lazytainer container**, so it owns your container networking. The signal is a
packet count rather than a connection, so a chatty health check keeps things awake and a quiet
long-lived session can look idle. There is no spec file, no per-service readiness, no exports.

### What this means for the claim above

| | wakes on any TCP | no runtime replacement | laptop-first | per-service spec | restores RAM |
|---|---|---|---|---|---|
| **sbx** | ● | ● | ● | ● | ○ |
| zeropod | ● | ○ shim + CRIU + eBPF | ○ | ○ | ● |
| Lazytainer | ● | ◐ owns networking | ● | ○ | ○ |
| Sablier | ○ HTTP only | ● | ● | ◐ labels | ○ |

The honest sentence is not "nobody does this". It is: **zeropod does it deeper and needs a
cluster; Lazytainer does it cruder and needs your network; Sablier does it for HTTP and is the
one most people actually run.** What is genuinely unoccupied is the intersection — arbitrary
TCP, no runtime to replace, a committed spec file, and the same binary on a laptop and in a
cluster.

That is a narrower claim than the one this file made in its first version, and it is the true
one.

### What we have actually measured, and what we have only read

Everything above about the three rivals comes from their documentation and source. As of
2026-08-15, `scripts/compare.sh` has tried to measure them on one machine, and this is the
state of that attempt:

| claim in this file | status |
|---|---|
| Sablier is HTTP-only — cannot wake a `psql` client | **measured**: reports `N/A` for the postgres target, by design, not by failure |
| Sablier's ~1.5–2 ms steady-state overhead | **unmeasured here** — its Traefik middleware would not engage under any plugin config tried, so no honest number was taken |
| zeropod restores in tens to a few hundred ms, with RAM intact | **unmeasured here** — no verified observable distinguishes checkpointed from running, so the arm produces no table rather than a number taken without that gate |
| Lazytainer wakes on any TCP | **measured** — it does, but it never holds the connection: attempts 1–5 refused, served on the 6th, 5150 ms later. 0/5 first attempts served against sbx's 5/5 |
| sbx wakes on raw TCP (postgres) | **measured** — 5/5 first attempts served, median 931 ms |
| sbx's proxy tax | **measured** — 33 µs/req over a same-container floor, ±21 µs, alongside benchstat's ~15 µs by a different method |
| the sbx daemon is 4.5 MB | **measured and wrong** — 9.1 MB at rest. Corrected in BENCHMARKS.md, the README and the architecture diagram |

Two of seven are still read rather than run — Sablier's overhead and zeropod's restore —
and one of the measurements refuted a claim of our own rather than a rival's.

The Lazytainer row is the one worth reading twice. "Wakes on any TCP" is true of it and
was the axis this document used to group it with sbx. Measuring it showed the grouping was
too kind: waking on a packet threshold and holding a connection are different products, and
only one of them works for a client that does not retry.

---

## Feature by feature

| | sbx | E2B | Daytona | Modal | Cloudflare | Fly | Neon | Northflank |
|---|---|---|---|---|---|---|---|---|
| Wakes on a raw socket | ● | ○ | ○ | ○ | ○ | ● | ◐ pg only | ○ |
| Runs on your laptop | ● | ○ | ○ | ○ | ○ | ○ | ○ | ○ |
| Same spec local + cluster | ● | ○ | ○ | ○ | ○ | ○ | ○ | ◐ |
| Self-hosted, no account | ● | ○ | ◐ OSS core | ○ | ○ | ○ | ○ | ◐ BYOC |
| Arbitrary stateful services | ● | ◐ | ● | ◐ | ◐ | ● | ○ pg | ● |
| Multiple services, one spec | ● | ○ | ○ | ○ | ○ | ◐ | ○ | ● |
| Zero cost at rest | ● | ◐ storage | ◐ storage | ◐ storage | ◐ storage | ◐ storage | ◐ storage | ○ |
| RAM-state snapshot | ○ | ● | ● | ● | ◐ | ● | n/a | ○ |
| VM-grade isolation | ○ | ● | ◐ | ● | ● | ● | ● | ● |
| Public URL per sandbox | ● | ● | ● | ● | ● | ● | n/a | ● |
| GPU | ○ | ◐ | ◐ | ● | ○ | ● | n/a | ● |
| Production-proven | ○ | ● | ● | ● | ● | ● | ● | ● |

### Rows chosen by them, not by us

The table above still asks "does everyone else do what we do". These are the ones their
documentation leads with, and this is where sbx has nothing rather than something partial.

| | sbx | E2B | Daytona | Modal | Cloudflare | Northflank |
|---|---|---|---|---|---|---|
| **Network egress control** — allow/deny by IP, CIDR, domain | ◐ `egress: deny`, all-or-nothing | ● wildcards | ● firewall | ● | ● | ● |
| **Fork N sandboxes from one snapshot** | ◐ `sbx fork`, filesystem only | ● 5–30 ms, with RAM | ● | ● | ◐ | ○ |
| **Prebuilt templates, versioned and cached** | ◐ five, embedded, not built | ● | ● 24 h cache | ● | ● | ● |
| **Declarative image builder** | ○ bring your own image | ◐ | ● | ● | ◐ | ● |
| **Interactive access: SSH · PTY · VNC** | ◐ `exec -t` gives a PTY; no SSH, no VNC | ◐ | ● all three | ◐ | ◐ | ● |
| **Volumes shared between sandboxes** | ○ one per service | ● NFS/block | ● subpath mounts | ● | ◐ | ● |
| **Language SDKs** (Python, JS) | ○ CLI only | ● | ● | ● | ● | ● |
| **Multiple regions / hosts** | ○ one machine | ● | ● | ● | ● | ● |

● yes · ◐ partial or conditional · ○ no

**Read that table before the one above it.** Eight rows, and sbx scores nothing on three of
them.

The first row was the most serious and is now half closed. `egress: "deny"` gives a service
no route off the host while leaving it reachable and wakeable — verified in both directions.
What it is not is what the rivals actually ship: allow and deny **by domain, CIDR and IP**.
All-or-nothing is a real control and a coarse one, and the gap between it and a wildcard
allow-list is where a filtering proxy would have to go.

The second is nearly as important and is not the same thing as CRIU: E2B can spawn *many*
sandboxes from one snapshot in tens of milliseconds. That is a different capability from
"resume the one you paused", and it is what makes per-task fan-out cheap for them.

`sbx snapshot` / `sbx fork` now do the fan-out half — many sandboxes from one saved state,
each with its own copy of the data — but the state is a filesystem, not a memory image. A
fork starts cold. Half a row, marked as half.

**Read the bottom two rows before the top two.** Every hosted platform on this table has real
isolation and real users. This has neither. What it has is the first four rows, and whether
that trade is right depends entirely on whether your sandbox is running your own services or
somebody else's code.

---

## Where to use something else

| If you need | Use | Why |
|---|---|---|
| To run genuinely untrusted code | **E2B, Vercel Sandbox, Modal** | Firecracker microVMs. A container shares your kernel |
| An agent's REPL resumed mid-thought | **E2B** | RAM snapshot. This restores disk only |
| A URL per pull request, for reviewers | **Northflank, Uffizzi, Okteto** | Built for the PR lifecycle, with teardown |
| Serverless Postgres, hosted, that's it | **Neon** | Branching and scale-to-zero, operated by someone else |
| Ephemeral fixtures inside a test run | **Testcontainers** | Ephemeral is the whole design; nothing to sleep |
| HTTP-only, already on Knative | **Knative** | Mature, and this is not |
| GPU on the sandbox side | **Modal** | No substitute at the moment |

---

## Sources

Vendor documentation, read August 2026:

- [E2B — sandbox persistence](https://docs.e2b.dev/sandbox/persistence): `sandbox.connect()` to
  resume; ~4 s per GiB to pause, ~1 s to resume; filesystem *and* memory state preserved.
- [Fly Proxy autostop/autostart](https://fly.io/docs/reference/fly-proxy-autostop-autostart/):
  "The proxy waits for a request to your app… starts a stopped or suspended Machine". Also:
  "only works on existing Machines and never creates or destroys Machines for you."
- [Fly — machine suspend and resume](https://fly.io/docs/reference/suspend-resume/)
- [Cloudflare Sandbox SDK](https://developers.cloudflare.com/sandbox/): `sleepAfter` idle sleep;
  [tunnels API](https://developers.cloudflare.com/sandbox/api/tunnels/) via `cloudflared`, which
  [replaced `exposePort()`](https://developers.cloudflare.com/changelog/post/2026-06-09-deprecating-sandbox-sdk-features/)
  in 2026 — the same conclusion this repo reached about its own tunnel, independently.
- [Knative — configuring scale to zero](https://knative.dev/docs/serving/autoscaling/scale-to-zero/)
  and [the activator on the data path](https://knative.dev/blog/articles/demystifying-activator-on-path/).
- [Neon — connection latency](https://neon.com/docs/connect/connection-latency): a few hundred ms
  from idle; scale-to-zero after 5 minutes by default.
- [Vercel Sandbox](https://vercel.com/docs/vercel-sandbox), [Modal sandboxes](https://modal.com/docs/guide/sandbox),
  [Daytona](https://www.daytona.io/docs/), [Northflank preview environments](https://northflank.com/blog/preview-environment-platforms).

The self-hosted prior art, read from the repositories themselves:

- [ctrox/zeropod](https://github.com/ctrox/zeropod) — containerd shim, CRIU checkpoint, eBPF
  activity tracking, restore "in tens to a few hundred milliseconds"; README notes arm64 in a
  Linux VM on macOS is "somewhat flaky".
- [sablierapp/sablier](https://github.com/sablierapp/sablier) — API server plus reverse-proxy
  middleware; docker, swarm, podman, kubernetes and Proxmox providers; blocking and dynamic
  strategies; ~1.5–2 ms steady-state overhead per request.
- [vmorganp/Lazytainer](https://github.com/vmorganp/Lazytainer) — packet-count thresholds on a
  monitored interface; "you must apply a label to them and proxy their traffic through the
  Lazytainer container".

Third-party benchmark figures (Daytona ~90 ms p99, Modal/E2B comparisons) come from published
2026 roundups rather than a harness in this repo, and are marked "reported" wherever they
appear. **They were not reproduced here**, and cross-platform latency numbers taken on
different hardware, in different regions, against different images are not a like-for-like
measurement. Ours are in [BENCHMARKS.md](BENCHMARKS.md) with the machine and the script beside
them; theirs are on their own machines.
