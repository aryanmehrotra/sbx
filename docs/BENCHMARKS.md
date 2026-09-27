# Benchmarks

Every performance figure sbx publishes, with the script, machine, version and date behind it.
For readers checking a claim and for contributors changing a hot path.

The page runs: [headline numbers](#headline-numbers) (one table, newest figures), then
[the v0.14.0 run](#v0140-on-linux-x86_64-cloud-vm-4-vcpu-xeon-21-ghz-2026-09-27) figure by figure,
then the method ([how to reproduce](#how-to-reproduce), [what CI measures](#what-the-pipeline-measures-and-what-it-does-not)),
then an [appendix](#appendix-earlier-runs-by-topic) of earlier runs and engineering notes.

## Headline numbers

**Re-measured on v0.14.0, 2026-09-27**, on a Linux x86_64 cloud VM (details in
[the v0.14.0 run](#v0140-on-linux-x86_64-cloud-vm-4-vcpu-xeon-21-ghz-2026-09-27)). Each row shows
that fresh figure beside the previous one, which is kept with its own machine and version. The two
are different machines, so a difference between them is not a speed-up or a regression. The
microVM rows could not be re-run there: it has no `/dev/kvm`.

| what | v0.14.0 · cloud VM · 2026-09-27 | previous figure · version · machine | script |
|---|---|---|---|
| first attempt served on wake, postgres: sbx vs Lazytainer | **20/20** vs 0/5 | 5/5 vs 0/5 · v0.1.0 · darwin/arm64, host load 5.37 | `scripts/compare.sh` |
| memory of a sleeping sandbox | **0 B**: no container running, 3/3 rounds | 0 B · v0.1.0 · laptop | `scripts/bench-memory.sh` |
| daemon RSS at rest, no sandboxes | **12.8 MB** (12.6-12.9, n=3 fresh daemons) | 9.1 MB · v0.1.0 · macOS laptop | `scripts/bench-memory.sh` |
| docker wake, redis | **216 ms** median, n=20, p90 236, stdev 14 | 191 ms · v0.1.0 · laptop | `scripts/bench.sh 20` |
| docker thaw from `"on_idle": "freeze"`, redis | **34 ms** median, n=20, p90 37; **12 ms** over the same ping awake (paired) | none: first measurement | `scripts/bench-freeze.sh 20` |
| docker wake, postgres / nginx | **348 ms** (p90 488) / **240 ms** (p90 261), n=20 each | 931 / 174 ms, n=5 · v0.1.0 · darwin/arm64, host load 5.37 | `scripts/compare.sh 20` |
| headless Chrome wake, cold / warm | **611 ms** / **387 ms**, n=5 each, alternating | 3744 / 766 ms, n=5 · v0.1.0 · macOS arm64 | `scripts/bench-chrome.sh` |
| `sbx create`, redis, image present | **362 ms** median, n=10, p90 375 | 492 ms, n=1 · v0.1.0 · laptop | `scripts/bench-create.sh` |
| OpenSandbox create → first command, docker warm pool, 1 at a time | **12.8 ms** median, n=10, p95 77.8 | 13.7 ms · v0.10.0 · Apple M4, colima 3 vCPU | `scripts/osb-bench.sh` |
| same, 100 at once from the pool | **309 ms** and **460 ms** median in two runs, n=100 each, p95 405 / 600 | 472 ms · v0.10.0 · Apple M4, colima 3 vCPU | `scripts/osb-bench.sh --burst 100` |
| a new connection to an awake sandbox, vs docker direct | **+0.17 ms** median (+0.15, +0.18, +0.17 in 3 runs of n=20) | +0.10 ms · v0.1.0 · laptop | `scripts/connbench.sh` |
| round trip on an open connection | **+9.6 µs** (9.4 → 19.1 µs), n=12 | +14 µs · v0.8.0 · Apple M4 | `go test -bench RoundTrip` |
| opening a connection, in-process | **+87 µs** (127 → 214 µs), n=10 | +53 µs · v0.8.0 · Apple M4 | `go test -bench Conn` |
| bulk throughput through the proxy, loopback | **1.39 GB/s**, 55% of direct (2.54 GB/s), n=10 | 7.0 GB/s, 56% of direct · v0.8.0 · Apple M4 | `go test -bench Stream` |
| kubernetes wake | not re-run: no cluster here | 1534 ms, n=5 · v0.1.0 · minikube | `scripts/bench.sh` |
| OpenSandbox create → first command, frozen microVM pool | not re-run: no KVM here | 144 ms · v0.14.0 · GitHub `ubuntu-24.04`, nested KVM, CI `microvm` job ([v0.14.0 notes](release-notes/v0.14.0.md)); 141 ms on the v0.13.0 tree (`e0749be`) | `osb-bench.sh --provider firecracker` (CI `microvm` job) |
| microVM wake from a Mac, through the helper VM | not re-run: no KVM here | 216 ms · v0.11.0 tree (`b21492f`) · Apple M4, colima nested. Stale: before the jailer (v0.13) and the layered root (v0.14) | `scripts/fc-anywhere-e2e.sh` |

**The cloud VM is noisier than a laptop, and it is still a VM.** Docker runs inside it, as it does
inside Docker Desktop's VM on a Mac, so none of this is bare metal. Its CPUs are slower per core
than an M4's, which is why the loopback benchmarks (round trip, bulk, open) are slower in absolute
terms while the ratios to direct stay close. The wake figures include each harness's own client
start: about 12 ms of the timer's own start in `bench.sh` (measured), plus `redis-cli`'s, and a paired baseline of 21 ms
(nginx) and 113 ms (postgres) in `compare.sh`.

**Not yet measured anywhere:** a microVM wake on bare-metal Linux (the only place the projected
4–28 ms, from published restore figures in
[the Firecracker spike](design/2026-09-26-firecracker-spike.md), can be confirmed), snapshot disk footprint per VM, a Windows/WSL2 host, kata-fc on
kubernetes, and concurrent microVM restores at N≥5 off nested virtualisation.

## v0.14.0 on Linux x86_64 cloud VM (4 vCPU Xeon 2.1 GHz), 2026-09-27

Every figure in the headline table's v0.14.0 column comes from this run. One machine, one
afternoon, the benchmarks run one after another and never at the same time.

| | |
|---|---|
| tree | v0.14.0 code, `8dff521` (later commits are docs only), `go build -o sbx .` |
| machine | cloud VM, 4 vCPU `Intel(R) Xeon(R) Processor @ 2.10GHz`, 15 GiB RAM, no swap |
| kernel | `6.18.44-fc-v37` |
| docker | 29.3.1, storage driver `overlayfs`, cgroup driver `cgroupfs`, cgroup v1 |
| Go | go1.26.0 linux/amd64 |
| not available | `/dev/kvm` (no microVM runs), kubernetes |

Images were pulled first (`docker pull`, `sbx prewarm --spec examples/browser/sandbox.json`), so no
timing includes a download. Host load was 0.1-1.9 at the start of each script.

### Wake, redis (v0.14.0)

`scripts/bench.sh 20`: median **216 ms**, min 191, p90 236, max 251, stdev
14 ms, 20/20 served. Timing starts a Python timer twice per sample; that alone costs 11-13 ms here.

### Freeze and thaw (v0.14.0)

`scripts/bench-freeze.sh 20`: the redis spec with `"on_idle": "freeze"`, so going idle runs
`docker pause` instead of a stop. Each run waits until docker reports the container paused, then
times one `redis-cli ping` from the host. Thaw: median **34 ms**, p90 37, min 28, max 40, 20/20
served. The same ping against the box awake, straight after: median 20 ms (17-27). That floor is
`redis-cli` start plus the timer, and it sits inside every thaw sample. Paired per run, the thaw
costs **12 ms** over it (median; p90 17, min 6, max 19). A first run gave 34 / 20 ms too.

### Wake against the field (v0.14.0)

`CONTENDERS=sbx scripts/compare.sh 20`, then
`CONTENDERS=lazytainer,sablier,zeropod scripts/compare.sh 5` (the rivals at the n the last run
used, to keep the run under an hour). Noise floor 215 µs/req ±99.

| contender | target | n | median | p90 / max | paired delta | first attempt served |
|---|---|---|---|---|---|---|
| **sbx** | nginx | 20 | **240 ms** | p90 261 | 216 ms | **20/20** |
| **sbx** | postgres | 20 | **348 ms** | p90 488, stdev 74 | 236 ms | **20/20** |
| Lazytainer | nginx | 5 | 3061 ms | max 4074 | 3037 ms | **0/5** |
| Lazytainer | postgres | 5 | 3407 ms | max 5585 | 3303 ms | **0/5** |
| Sablier | nginx | - | SKIPPED | - | - | could not be stood up; not diagnosed further |
| Sablier | postgres | - | N/A | - | - | HTTP-only by design |
| zeropod | both | - | omitted | - | - | no kubernetes here |

sbx's overhead on nginx measured 134 µs/req over a same-container floor, jitter ±34 µs (n=6
pairs). That is inside this harness's noise floor of ±99 µs, so it is not a result.

**`compare.sh` needed a fix to run here.** On native Linux its postgres client dialled
`host.docker.internal`, which is the bridge address (172.17.0.1). The daemon listens on
127.0.0.1 only, so every sbx postgres sample failed after its 60 s timeout. On Linux without
Docker Desktop the client now shares the host network and dials 127.0.0.1. Docker Desktop and
colima keep the old path. `scripts/compare_test.sh` passes 8/8.

### A new connection, awake sandbox (v0.14.0)

`scripts/connbench.sh 20`, three runs: +0.15 ms (IQR
+0.10 to +0.22), +0.18 ms (+0.08 to +0.30), +0.17 ms (+0.06 to +0.25). Slower through the daemon
in 17, 16 and 20 of 20 pairs. The script runs the two sides in the same order each pair; the three
runs agree, so the figure is **about +0.17 ms**.

### Headless Chrome (v0.14.0)

No script was recorded for the v0.1.0 figure, so this run added one, `scripts/bench-chrome.sh`:
`examples/browser`, `sbx serve --idle 5s`, woken by `curl /json/version`, 10 runs alternating.
Odd runs dropped the page cache first (`echo 3 > /proc/sys/vm/drop_caches`): cold median **611 ms**
(607-619). Even runs did not: warm median **387 ms** (368-413). The same `curl` against the awake
browser takes 18-20 ms. "Cold" here means page cache dropped; the v0.1.0 cold runs were first
touches in a session on a Mac, which is not the same condition.

### Create (v0.14.0)

`scripts/bench-create.sh 10`: `sbx create` of `bench.sh`'s redis spec, then `sbx rm`, 10 times.
Median **362 ms**, p90 375, min 345, max 391. Each sample is timed with two `measure_ms` calls
(`scripts/lib/measure.sh`), and that pair alone took 11-13 ms here (n=5); the script prints this
floor at the end. The 362 ms includes it.

### Memory (v0.14.0)

`scripts/bench-memory.sh`, three rounds, each with a fresh `sbx serve --idle 5s`: RSS at rest 12.8, 12.9 and
12.6 MB; fronting one sleeping redis sandbox 13.1, 13.4, 13.0 MB; after a wake, 1000 pipelined
PINGs and 50 new connections, 13.4, 13.7, 13.2 MB. While the sandbox slept, no container of it was
running in any round: **0 B**. At the end of the 20-run `compare.sh` arms the daemon was 16.4 MB.
A Linux amd64 binary is not the macOS arm64 one, so 12.8 against 9.1 MB is not growth.

### Proxy, in-process (v0.14.0)

`go test -run '^$' -bench <name> ./internal/daemon`, read with `benchstat`:

```
  RoundTrip  direct   9.43 µs ±3%   proxied  19.08 µs ±1%   +9.6 µs  (+102%)   -count 12
  Conn       direct 127.1  µs ±5%   proxied 214.3  µs ±4%   +87 µs   (+69%)    -count 10
  Stream     direct 2543 MB/s ±7%   proxied 1391 MB/s ±13%  55% of direct      -benchtime 30x -count 10
```

### OpenSandbox create → first command (v0.14.0)

`scripts/osb-bench.sh` on the default docker socket,
`node:22-slim`:

| run | mode | sandboxes | ok | median ms | p95 ms | p99 ms | score |
|---|---|---:|---:|---:|---:|---:|---:|
| burst 1 × 10, interleaved | **pool** | 10 | 10 | **12.8** | 77.8 | 77.8 | 99.62 |
| burst 1 × 10, interleaved | cold | 10 | 10 | 341.8 | 392.1 | 392.1 | 96.39 |
| burst 100, run 1 | **pool** | 100 | 100 | **309.2** | 404.7 | 405.8 | 96.53 |
| burst 100, run 2 | cold | 100 | 100 | 8970.5 | 17710.5 | 17717.3 | 6.18 |
| burst 100, run 3 | **pool** | 100 | 100 | **459.6** | 599.5 | 605.8 | 94.84 |

The two pool bursts differ by 150 ms, so a pool burst of 100 here is "300-460 ms", not either
number. One pool claim at a time is 12.8 ms, the M4's 11-14 ms. All 100 succeed in every burst.

### Not run on v0.14.0

Anything on microVMs (`--provider firecracker`, `fc-anywhere-e2e.sh`), since the
VM has no `/dev/kvm`; the kubernetes wake, since there is no cluster; and zeropod.

---

## How to reproduce

```sh
scripts/bench.sh 20                                  # wake latency, distribution
scripts/bench-create.sh 10                           # sbx create, redis, image present
scripts/bench-freeze.sh 20                           # thaw from on_idle "freeze", redis
scripts/bench-memory.sh                              # daemon RSS; 0 B while asleep
scripts/bench-chrome.sh                              # headless Chrome wake, cold and warm
go test -run '^$' -bench RoundTrip -count 12 ./internal/daemon   # proxy overhead, for benchstat
go test -run '^$' -bench Stream -count 10 ./internal/daemon   # bulk throughput
./sbx selftest                                       # the whole cycle, ~9s
./scripts/e2e.sh 3                                   # several sandboxes at once
./scripts/recovery.sh                                # kill the daemon, twice
./scripts/fork-e2e.sh                                # snapshot, fork twice, prove independence
./scripts/interrupt-e2e.sh                           # kill a volume copy mid-write, source stays intact
./scripts/soak.sh 600                                # endurance: fd/RSS flat under connection churn
scripts/compare.sh 20                                # sbx against the field
```

---

## What the pipeline measures, and what it does not

Every tag attaches a `bench.md` to its GitHub release: `RoundTrip` and `Stream`, ten runs each,
with the runner's cpu, memory, kernel and Go version written at the top.

**It does not touch the numbers on this page.** A GitHub runner is a shared, virtualised machine
whose neighbours are invisible, so its figures aren't comparable to a laptop's. The attached file
is for comparing one release against the one before it on the same shape of machine — the question
a benchmark in a pipeline can actually answer.

Wake latency is deliberately not in it: it needs containers and is dominated by whatever else the
runner is doing, and a number that noisy on every tag teaches people to ignore the file it's in.
`scripts/bench.sh` measures that one, on a machine you can describe.

---

## Appendix: earlier runs, by topic

Each section below holds the runs before v0.14.0 for one figure, with the machine and version
each was measured on, and the engineering notes behind the numbers. They are kept, labelled,
not replaced: a figure measured on another machine is not a regression or a speed-up.

### Wake

| | median | detail | measured on |
|---|---|---|---|
| docker | **216 ms** | n=20, p90 236 ms, stdev 14 ms | v0.14.0 · Linux x86_64 cloud VM · 2026-09-27 |
| docker | 191 ms | n=20, p90 232 ms, stdev 24 ms | v0.1.0 · laptop · 2026-08-15 |
| kubernetes | **1534 ms** | n=5, min 1362, max 2060 - a pod must be scheduled | v0.1.0 · minikube · 2026-08-15, not re-run |

**Both assume a declared `health` command.** Without one there is nothing to ask - docker
binds a published port the instant the container starts, so dialling it proves nothing - and
the daemon waits a flat **2 s** before letting the caller through. That is ten times the
number above, on a configuration the spec permits, which is why every bundled template
declares a health check and why SPEC.md calls it close to required.

The docker wake was re-measured on v0.14.0 with `scripts/bench.sh 20` on a Linux cloud VM
([the run](#v0140-on-linux-x86_64-cloud-vm-4-vcpu-xeon-21-ghz-2026-09-27)). The 191 ms is kept with
its machine; the two are different machines, so the gap between them is not a regression. The
kubernetes figure has not been re-run since v0.1.0 and is stale: the wake path changed in v0.13. For scale against hosted platforms, see
[against other platforms](#against-other-platforms) below.

#### Why wake is fast

Two things make these numbers hold up.

**A redundant probe is gone from the path.** The wake used to ask the workload whether it was
serving *before* starting it, to catch a container somebody had started outside sbx. But the
unit being asleep is the reason wake was called in the first place, so on its real path that
probe only ever added a round trip - and starting an already-running container is a 304 the
provider already treats as success, answering the same question `Start` answers for free.
Removing it took one round trip off every cold wake, held by a test that counts probes: a cold
wake costs exactly one now.

**The remaining cost is docker's own health-check cadence, and sbx sidesteps it.** Waiting on
docker to notice a container is healthy means waiting on its polling interval; running the
declared health command directly instead gets the answer as soon as it's true. That's what
gets wake down to:

```
   wake     216 ms        (v0.14.0, Linux cloud VM, 2026-09-27; 191 ms at v0.1.0 on a laptop)
   create   362 ms        (v0.14.0, same run; 492 ms at v0.1.0)
   cluster 1534 ms        (v0.1.0, minikube, not re-run)
```

---

### A heavier workload: headless Chrome

Redis is the wake benchmark because it isolates the wake path from the workload's own
startup. Chrome is the other end of the range - the browser template, woken by a plain CDP
request:

```
  cold   run 1  4356 ms    run 2  3744 ms    run 3  3030 ms
  warm   run 4   703 ms    run 5   829 ms
```

Cold median **3744 ms**, warm median **766 ms** (n=5, macOS arm64, v0.1.0, 2026-08-16).
On v0.14.0 on a Linux cloud VM (2026-09-27), alternating cold and warm runs gave cold **611 ms**
and warm **387 ms** (n=5 each). There "cold" means the page cache was dropped, not a first touch
in a session, so the two cold figures are not the same condition; see
[the run](#v0140-on-linux-x86_64-cloud-vm-4-vcpu-xeon-21-ghz-2026-09-27). Layer and page-cache
warming bring later runs down within a session, so the number to plan around is two regimes:
seconds on first touch, well under a second once the image is warm. The cost here is Chrome's
own startup - the same image started by hand costs the same - which is the point: sbx removes
the cost of a browser nobody is using, not the cost of starting one.

---

### OpenSandbox create → first command (ComputeSDK's Burst TTI)

ComputeSDK ranks hosted sandboxes on **TTI: client-timed `create()` → first successful
`runCommand('node -v')`, 100 launched at once**, scored 0.6·s(median) + 0.25·s(p95) +
0.15·s(p99) with s(ms) = 100·(1 − ms/10000), times the success rate
([computesdk/benchmarks](https://github.com/computesdk/benchmarks), METHODOLOGY.md). The same
thing, through the upstream OpenSandbox Go SDK (`CreateSandbox` = POST, GET until Running, the
execd endpoint, `/ping`; then `RunCommand`):

```sh
scripts/osb-bench.sh --docker-host unix://$HOME/.colima/osb/docker.sock --burst 1 --rounds 10 \
  --pool node:22-slim=8 --burst-modes default,cold          # one at a time, pool vs cold, interleaved
scripts/osb-bench.sh --docker-host unix://$HOME/.colima/osb/docker.sock --burst 100 --rounds 1 \
  --pool node:22-slim=100                                   # from the pool
scripts/osb-bench.sh --docker-host unix://$HOME/.colima/osb/docker.sock --burst 100 --rounds 1 \
  --burst-modes cold                                        # no pool
```

Apple M4, 16 GiB; a dedicated colima profile with **3 vCPU / 3 GiB**, docker 29.2.1;
`node:22-slim` pre-pulled. Measured 2026-09-26. **A local number, not a leaderboard entry**:
ComputeSDK's runner measures a hosted endpoint over the internet.

#### Where a create's 4 seconds went

`SBX_OSB_TRACE=1` logs each phase of a create, in ms from the POST being accepted. One create at
a time, before (09f3db2) and after:

| phase | before | after, cold | after, from the pool |
|---|---:|---:|---:|
| `docker pull` of an image already present | **2803-3185** | skipped | - |
| image inspected | 2853-3223 | 19 | - |
| container created (`docker run`) | 3182-3249 | 158 | - |
| execd answers through the wake port → Running | 3202-3260 | 169 | - |
| create answered | 1 (Pending) | 170 (**Running**) | ~10 (Running) |
| the SDK's GET sees Running | **4019-4025** | 176 | ~26 |
| execd endpoint answered | 4030-4036 | 182 | ~38 |
| **TTI** (client, `node -v` done) | **4039, 4048** (n=2; a first create seeding the execd volume: 6042) | 227 median (n=10) | **11.1 median** (n=10) |

The 4 s was two fixed costs stacked:

- An unconditional `docker pull` asked the registry for a manifest the engine already had (~3 s).
  sbx now pulls only a missing image, as upstream's docker runtime does.
- The SDK's `waitForRunning` polls GET every **2 s**, so a sandbox Running at 3.2 s was seen at
  the 4 s poll. sbx now holds the create's answer until Running (up to 20 s), so the SDK's first
  GET ends its wait.

#### Burst

| run | mode | sandboxes | ok | median ms | p95 ms | p99 ms | score |
|---|---|---:|---:|---:|---:|---:|---:|
| burst 1 × 10, interleaved | **pool** | 10 | 10 | **11.1** | 18.3 | 18.3 | **99.86** |
| burst 1 × 10, interleaved | cold | 10 | 10 | 227.0 | 288.5 | 288.5 | 97.49 |
| burst 100, run 1 (cold first) | cold | 100 | 100 | 8072.7 | 16169.2 | 16447.3 | 11.57 |
| burst 100, run 2 | **pool** | 100 | 100 | **412.6** | 482.9 | 504.7 | **95.57** |
| burst 100, run 3 | **pool** | 100 | 100 | **432.3** | 517.5 | 520.5 | **95.34** |
| burst 100, run 4 | cold | 100 | 100 | 46720.4 | 79832.1 | 80213.3 | 0.00 |
| burst 1 × 10, re-key every claim | **pool** | 10 | 10 | 13.7 | 41.2 | 41.2 | 99.76 |
| burst 1 × 10, same run | cold | 10 | 10 | 207.5 | 356.0 | 356.0 | 97.34 |
| burst 100, re-key every claim | **pool** | 100 | 100 | 472.1 | 566.9 | 573.2 | 94.89 |

The table above is the M4 (v0.10.0 onward). v0.14.0 on a Linux x86_64 cloud VM gave a pool claim
of 12.8 ms one at a time and 309 / 460 ms for two pool bursts of 100; see
[the run](#v0140-on-linux-x86_64-cloud-vm-4-vcpu-xeon-21-ghz-2026-09-27).

Runs 1-4 alternate cold/pool/pool/cold, each against a fresh daemon. The two cold runs differ by
6x - the second followed two pool runs that had just made and removed 200 containers - so the
cold burst is *not resolvable* beyond "seconds to tens of seconds"; what is resolvable is that
all 100 succeed where 09f3db2 had 60 slots. The two pool runs agree within 20 ms.

The last three rows are after every claim re-keys execd (runs 2-3 re-keyed only when the create
carried env). At one at a time the difference is inside the spread (5.7-41.2 ms): not resolvable.
At 100 it is ~40-60 ms of median, one round trip through the wake port per claim.

What bounds each path here:

- **Pool, burst 100**: a claim touches no container (members wait pinned running, not frozen:
  twenty concurrent `docker unpause`s measured 200-430 ms, serialised in dockerd). The server has
  answered every GET and endpoint by ~160 ms; the rest is 100 `node -v` processes starting at
  once on 3 vCPUs, plus colima's port forward on every new connection. A single claim is 11 ms.
- **Cold, burst 100**: `docker run` throughput - ~150 ms of engine time per container, 8 in
  flight - after the lock convoys were removed (slot choice, discovery, image inspect, record
  writes; see the commits on `osb/speed`).
- **Memory**: 50 waiting members held ~620 MiB of the 3 GiB VM (free: 922 MiB used vs ~300
  idle), so a pool of 100 plus a burst of 100 claimed fits; 200 cold containers on top would not
  be attempted here.

---

### OpenSandbox create → first command on microVMs (v0.13)

The same Burst-TTI shape as above, with every sandbox a Firecracker microVM, run by CI's
`microvm` job on each change. That job ran on a GitHub-hosted `ubuntu-24.04` x86_64 runner with
`/dev/kvm`, with the jailer on. These are **nested-virtualisation numbers on a shared runner**:
they are for comparing one commit with another, not for a leaderboard, and no bare-metal run
exists yet.

```sh
# what the microvm job runs (ci.yaml), once with members asleep and once frozen
scripts/osb-bench.sh --provider firecracker --burst 4 --rounds 3 --pool node:22-slim=4 \
  --burst-modes default,cold          # add --pool-freeze for frozen members
```

Measured at `e0749be` (the v0.13.0 tree less its release notes), run
[36251708713](https://github.com/aryanmehrotra/sbx/actions/runs/36251708713): 3 rounds of
4 concurrent creates, `node:22-slim`, modes interleaved and rotated per round.

| mode | n | median | p95 | min | max | `create()` median | `node -v` median |
|---|---:|---:|---:|---:|---:|---:|---:|
| cold, no pool | 12 | 2,821 ms | 5,769 ms | 1,386 | 5,769 | ~2,650 ms | ~65 ms |
| pool, members **asleep** | 12 | 699 ms | 957 ms | 462 | 957 | ~260 ms | **320–640 ms** |
| pool, members **frozen** | 12 | **141 ms** | 207 ms | 121 | 207 | **~37 ms** | ~94 ms |

On v0.14.0 the same job gave **144 ms** frozen, 759 ms asleep and 2,637 ms cold, unchanged within
runner noise ([v0.14.0 release notes](release-notes/v0.14.0.md)). `node:22-slim` is small, so it
cannot show the layered root's gain, which is in large images.

All 24 pooled creates were served from a member: the script prints that count (`creates answered
from the warm pool: 12` in each mode) but does not fail on zero, so read it before trusting a row.

**What the split says:**

- **Cold is the create.** A cold microVM spends ~2.6 s in `create()` (image to rootfs, boot,
  execd up) and then runs `node -v` in ~65 ms, like any booted machine.
- **Asleep moves the cost to the first command.** Claiming an asleep member is a snapshot load, so
  `create()` returns in ~260 ms, but its memory is paged in lazily: the first `node -v` then takes
  320–640 ms, about five times the booted figure. The TTI is honest about this; a `create()`-only
  number would not be.
- **Frozen is paused in RAM.** The member was never written out, so `create()` is a re-key over
  vsock (~37 ms) and `node -v` runs at close to booted speed. The price is that a frozen member
  **holds its RAM** while it waits, which an asleep one does not.

The docker figures above (13.7 ms from a pool, every claim re-keyed) are a container that is already running. A
microVM from a frozen pool is **141 ms on this runner** at v0.13.0 and 144 ms at v0.14.0, with its own guest kernel and a jailed VMM.

---

### A Firecracker microVM on a Mac, through the helper VM

> **Stale.** Measured on the v0.11.0 tree, before the jailer (v0.13) and the layered root disk
> (v0.14). The 2.3 s root-filesystem clone in the create below is the step v0.14's layered root
> replaces with a hard link, so the create figure in particular will not reproduce.

`--provider firecracker` from macOS: the sandbox is a Firecracker microVM inside a Linux helper
VM with nested virtualisation; asleep is a snapshot on disk, a wake is `snapshot/load` + resume +
an execd re-key over vsock, and every sleep after the first is a Diff folded into the base.

```sh
go build -o sbx . && SBX_FC_VM_DRIVER=colima FC_E2E_ROUNDS=12 scripts/fc-anywhere-e2e.sh
```

Apple M4, 16 GiB, macOS 26.4.1; helper VM: colima 0.10.1 profile `sbx-fc-e2e`, `--vm-type vz
--nested-virtualization`, **2 vCPU / 2 GiB**, created by the script and deleted after it (two
other colima VMs, 4 GiB and 3 GiB, running throughout). Firecracker v1.17.0, the CI 6.18 kernel,
`nginx` template (it declares a `health` command), 1 vCPU / 256 MiB guest. Re-measured 2026-09-26 at
b21492f, one run of 12 rounds, each round a
sleep, a wake, an awake request and an exec (those two alternating order) and a create + rm of a
second sandbox (before or after the wake, alternating). All times are client-side, on the Mac:

| | n | median | p95 |
|---|---:|---:|---:|
| **wake from snapshot → first byte** (TCP connect on the Mac, HTTP 200) | 12 | **216 ms** | 256 ms |
| request to an awake sandbox → first byte | 12 | 8.3 ms | 9.5 ms |
| `sbx exec` round trip (ssh into the helper VM + sbx + execd over vsock) | 12 | 725 ms | 908 ms |
| `sbx sleep` (Seal, pause, Diff snapshot, merge) | 12 | 680 ms | 811 ms |
| `sbx create` (image already pulled: export, ext4, cold boot, health, Full snapshot) | 12 | 11.5 s | 14.3 s |

**A regression, measured and fixed.** Between the first run (212 ms, before the spec's `health` ran
inside the VM) and b21492f, the wake median was **394 ms** (n=10, p95 577 ms): the daemon's
readiness probe ran the health command through execd on every wake. A traced build put each phase
in the in-VM daemon's log (n=6): snapshot load 3.5-10 ms, execd re-key 139-217 ms, **health
177-336 ms**, woke in 340-585 ms. A snapshot is of a workload already serving, so the command now
runs at create (before the snapshot) and after a cold boot only, and a snapshot wake dials the
first port.

The wake after the fix, by phase, from the same traced build (in the VM daemon's log, n=15; the Mac
saw first byte at a median 244 ms, p95 301 ms, n=12, in that traced run):

| wake phase (in the helper VM) | median | p95 |
|---|---:|---:|
| snapshot load + resume | 11.5 ms | 16.1 ms |
| execd re-key over vsock | 170.4 ms | 203.5 ms |
| readiness probe (first port) | 10.7 ms | 20.8 ms |
| **daemon: woke** | **206 ms** | 252 ms |

The re-key is now most of a wake: a vsock dial and handshake into a guest whose pages are still
faulting in under nested virtualisation.

And a create, by phase, inside the provider (traced, n=4, `nginx`, image already pulled; the
client-side totals for these four were 7.2, 14.1, 14.2 and 14.1 s):

| create phase | median | range |
|---|---:|---:|
| checks, lock, artifact lookup | 2.1 ms | 1.7-3.9 ms |
| root filesystem (cached by image ID) + agent binary | 50 ms | 45-73 ms |
| clone the root filesystem into the VM directory | 2.30 s | 2.16-2.35 s |
| agent drive (ext4 with `/sbx` + `/init.json`) + record | 93 ms | 71-139 ms |
| cold boot (launch, configure, InstanceStart) | 108 ms | 87-141 ms |
| boot to first port accepting | 2.41 s | 2.38-2.68 s |
| health command passing | 144 ms | 132-152 ms |
| Seal, pause, Full snapshot, kill | 632 ms | 483-793 ms |
| **sum inside the provider** | **~5.7 s** | |

The rest of the 11.5 s median - 1.5-8.5 s per create here - is outside the provider: ssh into the
helper VM, the in-VM CLI and its docker calls, and the redirect. It was not split further; the
first create in a run was 7.2 s and the next three 14.1-14.2 s, which is the part to look at
next. A 2.3 s clone is a byte copy, not a reflink (which is near-instant); which one ran is recorded
as `clone` in each VM's `vm.json` and was not read for this run.

Inside the helper VM, the provider alone (`SBX_FC_E2E=1`, `redis:7-alpine`, n=2, so read, not
ranked): restore + re-key 282-304 ms, first byte 326-342 ms, exec over vsock 265-326 ms, stop as
a Diff snapshot 75-84 ms, create 5.8 s.

Where it goes:

- **The Mac adds little to a wake.** 216 ms from the Mac against 206 ms for the daemon's own wake
  in the VM: the mirror and the ssh tunnel are not what a wake waits on. The spike's 88 ms was a bare `/init` restored
  and asked over vsock; here execd is re-keyed before the wake proxy lets a byte through, and the
  workload's pages fault in under nested virtualisation (spike: ~250-300 µs per stage-2 fault).
- **An exec is mostly process start-up and ssh.** In the VM an exec is ~300 ms - the vsock
  handshake, execd's `/command` and a fork in a nested guest; the other ~400 ms is `sbx` starting
  on the Mac and again in the VM over ssh.
- **Before this was measured, every wake cost 2.27 s**: the provider reported its port probe as
  undeclared, so the daemon waited a flat 2 s for the workload. A VM port has no proxy in front
  of it, so an accepted connection is a listener and the check is now declared.

Not measured: bare-metal Linux (the only place the projected 4-28 ms from
[the Firecracker spike](design/2026-09-26-firecracker-spike.md) can be confirmed or refuted),
a Windows/WSL2 host, kata-fc on kubernetes, and concurrent restores (the spike's N≥5 collapse
applies unchanged).

---

### Proxy overhead

The wake is a one-off; this is the tax on every query for the life of the sandbox.

```
             │ direct      │ proxied     │
             │ sec/op      │ sec/op      vs base
RoundTrip-10   12.52µ        26.78µ       +113.8%  (median of 5, 2026-08-31)
```

On v0.14.0, on a Linux x86_64 cloud VM (2026-09-27, `-count 12`), it was 9.43 µs direct and
19.08 µs proxied: **+9.6 µs**, +102%.

**About 14 µs on the M4, 10 µs on the cloud VM.** That's +114% against a bare loopback echo, or +7% against a real query that
already crosses a VM boundary at 426 µs - the baseline you pick changes the headline, so both
are given here rather than just the flattering one. Against the workloads sbx actually fronts,
where a query already costs hundreds of microseconds before it reaches the proxy, an extra
14 µs is not something a client will notice.

**This measures round trips on a connection that is already open.** What a client pays to
*open* one is a separate number, covered below.

---

### Throughput

The proxy overhead above is a latency figure on a six-byte PING. The workloads sbx actually
fronts are databases and browsers: a `pg_dump`, a `COPY`, a large result set, a CDP screenshot -
so it's worth knowing what sitting in that path costs on real transfer volumes, not just pings.

`go test -bench Stream -benchtime 30x -count 10`, 16 MiB per iteration, loopback:

```
  direct    n=5    median 12418 MB/s
  proxied   n=5    median  7011 MB/s          (2026-08-31)
```

On v0.14.0, on a Linux x86_64 cloud VM (2026-09-27, n=10), it was 2543 MB/s direct and
**1391 MB/s** proxied: 55% of direct. That CPU is slower per core than an M4, so the absolute
figure is lower and the ratio is the same.

**7.0 GB/s is the M4 figure; 6.8 GB/s is older.** The v0.7.0 release notes quote 6.8 GB/s
(57% of direct, n=10, 2026-08-21). The benchmark was re-run on 2026-08-31 for v0.8.0 (n=5) and gave
7011 MB/s against 12418 MB/s direct, which is the figure above. The two agree within their spread;
quote 7.0 GB/s. The 4.9 GB/s in the relay-buffer sweep below is a different benchmark
(`StreamBuf/64KiB`, `-count 6`) and is not a replacement for either.

**A bulk transfer runs at about 56% of direct on loopback.** Even at that rate, 7.0 GB/s is an
order of magnitude above what a Postgres `COPY` or similar workload actually produces, so the
database stays the binding constraint, not the proxy. It would only start to matter for
something that genuinely streams at memory speed.

#### The relay buffer: pooled, and bigger

The proxy copies each direction of a tunnel through a byte buffer. Two changes to that buffer
are worth calling out on their own.

**Pooled - zero allocation per connection.** The old code allocated a fresh buffer on every
connection (two directions per connection), which under many short-lived connections - a pool
with no reuse, an agent hammering redis with 162 of them - meant steady allocation and GC
pressure. The buffer now comes from a `sync.Pool`:

```
`go test -bench RelayBufAcquire -benchmem`
  pooled          8 ns/op        0 B/op   0 allocs/op
  make-per-conn   5995 ns/op   65536 B/op   1 allocs/op
```

Zero allocation per connection, against a full buffer freshly allocated and zeroed before -
pure CPU savings, so it holds up on a busy machine.

**Sized at 64 KiB - and the reason is memory, not throughput.** A bigger buffer drains a
loopback socket in fewer read/write syscalls, and it keeps paying well past 64 KiB. The sweep
used to stop at 256 KiB, which is inside the climb, so it could not see its own knee; extended
to 1 MiB on an Apple M4 (`-count 6`, medians, MB/s):

```
  StreamBuf/32KiB     4440
  StreamBuf/64KiB     4920   ← what ships
  StreamBuf/128KiB    5150   ← +5%
  StreamBuf/256KiB    6800   ← +38%
  StreamBuf/512KiB    7010   ← +42%, the knee
  StreamBuf/1024KiB   6870
```

An earlier version of this section reported 64 KiB as the fastest and 128/256 KiB as "slower
and wildly variable", measured in a reserved four-core slice. That does not reproduce here
under either condition - at `GOMAXPROCS=4` on this machine 64 KiB was the *slowest* row of the
sweep. Treat the curve as machine-specific and re-measure before trusting it.

**64 KiB ships anyway, and this is the trade.** The buffer comes from a pool holding two per
concurrently-live connection, so its cost scales with concurrency rather than with churn.
Measured against the daemon's own RSS, 60 concurrent streams:

```
  64 KiB    15 MiB idle → 23 MiB peak
  256 KiB   15 MiB idle → 33 MiB peak     +10 MiB
```

`scripts/soak.sh` shows no difference between the two (16→19 MiB either way) because it drives
connections mostly one after another - which is the measurement to distrust here, not the one
to quote.

So the larger buffer buys 38% more throughput for more than double the daemon's resident size
under load. It is not worth it *for this tool*: at 4.9 GB/s the proxy is already an order of
magnitude past what a Postgres `COPY` produces, so the database is the binding constraint and
the extra bandwidth is spent on nothing, while the memory is spent on a daemon whose whole
claim is that an idle sandbox costs nothing. A tool whose workload actually streams at memory
speed should raise it; `relayBuf` is one constant.

**One avenue this deliberately leaves on the table.** On Linux, `io.Copy` between two
`*net.TCPConn` can reach `splice(2)` and skip the userspace copy entirely - but the per-chunk
`touch()` that records activity defeats the type assertion `splice` depends on, and these
numbers are macOS, where `splice` does not exist anyway. Worth a Linux measurement.

The blame on `touch()` is right about the mechanism and wrong about the cost, which is worth
knowing before anyone spends a week on it: profiled, `touch()` is 32.33 ns - `time.Now()` at
28.45 plus an atomic store at 1.80 - which over a 16 MiB stream in 64 KiB chunks is 8.28 us of
a 2714 us iteration, 0.31%, and it appears in zero CPU samples. It is not in the way because it
is expensive. It is in the way because it is structural.

---

### A new connection to an awake sandbox

A client that opens a connection per operation (`psql`, `redis-cli`, any CLI, anything without
a pool) is the common case sbx is built for: the sandbox is already awake, so there's no wake
to pay, and each operation is a fresh connection, so the round-trip figure above doesn't
describe it either.

`scripts/connbench.sh`, interleaved against the same awake container, n=20 each side:

```
  through the daemon     median   0.79 ms   min 0.67   max 5.60
  straight to docker     median   0.69 ms   min 0.56   max 1.47

  per-connection cost   median +0.10 ms   IQR [-0.03, +0.21]
```

On v0.14.0, on a Linux x86_64 cloud VM (2026-09-27), three runs of n=20 gave +0.15, +0.18 and
+0.17 ms, IQRs clear of zero.

**A new connection to an already-awake sandbox costs about +0.1 to +0.2 ms** over dialling docker
directly. The daemon treats a unit it woke and hasn't slept as awake and serves the connection
without re-checking the workload's health on every dial - and that belief is verified
optimistically: if the container was stopped from outside, the upstream connect fails, the
belief is revoked, and a proper wake runs. Both paths are covered by tests in
`internal/daemon/awake_test.go`.

---

### Opening a connection, measured in-process

The figure above is a docker-backed one and includes everything. This is the same question asked
of the proxy alone, on loopback, with no container in the path — `BenchmarkConnDirect` against
`BenchmarkConnProxied`, one dial, one exchange, one close per iteration:

```
  ConnDirect     58.23 µs
  ConnProxied   111.00 µs        +52.8 µs   (+90.6%, median of 5, 2026-08-31)
```

On v0.14.0, on a Linux x86_64 cloud VM (2026-09-27, n=10): 127.1 µs direct, 214.3 µs proxied,
**+87 µs** (+69%).

**About 53 µs to open one on the M4, 87 µs on the cloud VM.** It is the number to watch when the dial path changes, because the
round-trip benchmarks hold one socket open for the whole run and amortise this to nothing — which
is exactly backwards for the clients sbx is built for. `psql`, `redis-cli` and any CLI without a
pool pay it on every operation.

#### This benchmark could not run past 16,384 iterations, and said "connection reset by peer"

Worth recording, because the failure named nothing useful and the cause was not in sbx.

`connChurn` closes its client sockets with a zero linger — RST rather than FIN — precisely so
they do not pile up in TIME_WAIT and exhaust the ephemeral range. The **daemon's upstream**
socket had the same problem and was missed: the daemon is the active closer there, so it takes
the TIME_WAIT, one per proxied connection, held for 2·MSL.

macOS has **16,384 ephemeral ports** (49152–65535). At ~113 µs per iteration the daemon opens
roughly 8,700 upstream sockets a second, so the range was gone in under two seconds:

```
  10,000 iterations   ok
  20,000 iterations   FAIL   read: connection reset by peer
                             16,367 sockets in TIME_WAIT   <- the whole range
```

The echo server now answers the daemon's FIN with an RST, which aborts the connection instead of
moving it to TIME_WAIT. **100,000 iterations pass with 5–8 sockets in TIME_WAIT.** The benchmark
measures the daemon again rather than the kernel's port table.

It is a test-only change. The same ceiling is real for any TCP proxy under that much connection
churn, and it is a property of the platform's port range rather than of sbx.

---

### Memory

The table was measured at v0.1.0 (2026-08-15) on a macOS laptop. The daemon and sleeping-sandbox
rows were re-measured on v0.14.0 on a Linux x86_64 cloud VM (2026-09-27, 3 fresh daemons): 0 B
asleep, **12.8 MB** at rest, 13.0-13.4 MB fronting one sleeping sandbox, 13.2-13.7 MB after
traffic. A Linux amd64 binary is not the macOS arm64 one, so this is not growth. Both containers fresh, both idle, same image - which is the only comparison that means
anything:

| | stock | tuned |
|---|---|---|
| `mysql:8.0` | 411 MB | **110 MB** |
| `clickhouse:24.3` | 199 MB | 201 MB |
| a sleeping sandbox | - | **0 B** |
| the daemon, at rest | - | **9.1 MB** |
| the daemon, fronting one sandbox | - | 9.6 MB |
| the daemon, after traffic | - | 10.4 MB |

Measured by `ps -o rss`: 9.1 MB with no sandboxes at all, 9.6 MB fronting one, 10.4 MB after a
wake and some traffic. The growth is small and bounded - about half a megabyte to front a
sandbox, and the rest is buffers that traffic touches.

MySQL's saving is real and comes from `performance_schema=OFF` and a 48 MB buffer pool.
ClickHouse is idle at about 200 MB either way - its cache caps pay off under load, not at rest.

---

### Against other platforms

Vendor-documented figures, read August 2026, beside ours - useful context, not a controlled
benchmark. The differences below explain why. Vendor cells are kept as read then;
[COMPARISON.md](COMPARISON.md) is the page that keeps them current.

| | idle → serving | what comes back | measured by |
|---|---|---|---|
| **sbx** docker | **216 ms** (v0.14.0, Linux cloud VM); 191 ms (v0.1.0, laptop) | disk warm, process cold | `scripts/bench.sh 20`, this repo |
| **sbx** kubernetes | **1534 ms** (v0.1.0, stale) | disk warm, process cold | `scripts/bench.sh`, minikube |
| **sbx** firecracker, Mac via helper VM | **216 ms** (stale, v0.11.0 tree) | **RAM + processes** | `scripts/fc-anywhere-e2e.sh`, [above](#a-firecracker-microvm-on-a-mac-through-the-helper-vm) |
| E2B resume | ~1000 ms | **RAM + processes** | [vendor docs][e2b] |
| Neon | a few hundred ms | Postgres data | [vendor docs][neon] |
| Fly, suspended | a few hundred ms | RAM snapshot | [vendor docs][fly] |
| Fly, stopped | ~2000 ms+ | disk | [vendor docs][fly] |
| Daytona | *none published* | disk, persistent volume | - |
| Knative | pod schedule, seconds | volume if attached | - |

[e2b]: https://docs.e2b.dev/sandbox/persistence
[neon]: https://neon.com/docs/connect/connection-latency
[fly]: https://fly.io/docs/reference/suspend-resume/

#### Why these numbers don't belong in the same table

They're here because people ask, and refusing to answer is its own kind of unhelpful. But four
things make the column non-comparable, and all four favour us:

| why it is not like-for-like | |
|---|---|
| **Different hardware** | ours is one laptop with nothing else running; theirs is a multi-tenant fleet |
| **Different distance** | ours is loopback. Theirs crosses the internet, and the [Neon docs][neon] name cold start as the primary cause with distance a further factor on top |
| **Different images** | a wake is mostly the workload's own start-up, so redis and a Firecracker microVM are not the same measurement |
| **Different definitions of awake** | ours is a correct protocol reply. Theirs is whatever their page counts |

The one comparison that *is* fair is structural rather than numerical: **what has to happen for
the wake to start at all.** On every hosted platform it's an SDK call from code that knows the
sandbox exists; here it's the client's own socket. That's in
[COMPARISON.md](COMPARISON.md), and it doesn't depend on anyone's hardware.

---

### Against the field, measured here

`scripts/compare.sh` runs sbx and its self-hosted rivals against the same targets on one
machine. It answers the obvious objection to the table above: every rival figure in it was
read rather than measured.

```sh
scripts/compare.sh 20                          # all contenders, both targets
CONTENDERS=sbx,sablier scripts/compare.sh 5
```

**It publishes almost nothing, on purpose.** Three rules decide whether a sample may become a
number, and each exists because of a specific way this kind of benchmark can mislead:

| rule | why |
|---|---|
| a sample counts only on a **correct protocol reply** | Sablier's middleware failed to engage during development and returned **502 in 98 ms** - faster than sbx's real wake. A status code is not evidence |
| a sample is **VOID** unless the target was verifiably asleep at `t0` | otherwise a rival whose mechanism never engaged scores a spectacular wake for answering while already awake |
| every wake is **paired** with a baseline through the identical client | the first real run showed ~100 ms of each 336 ms "wake" was `curl`'s own startup |
| overhead is measured against **the same container without the wake path**, interleaved | run in blocks, load drift lands in the answer: this floor moved 660 µs → 4280 µs between two runs |
| a delta inside the harness's own jitter is **not published as a number** | the jitter here is ±150-900 µs and the proxy tax is ~15 µs, so this harness cannot resolve it and says so |

`N/A` and `SKIPPED` are different facts. Sablier has no postgres row because it is HTTP-only
by design - that is a *result*.

Correctly gating zeropod required scraping its `zeropod_running` metric directly (0 when
checkpointed), since the pod stays `Running` while checkpointed and `kubectl get pod` alone
can't tell asleep from awake. `scripts/zeropod-probe.sh` does that scrape from inside the
cluster, which is what produced the 272 ms row below.

#### Measured · 2026-09-27, v0.14.0

On a Linux x86_64 cloud VM, host load 0.3, noise floor 215 µs/req ±99. Full table in
[the v0.14.0 run](#v0140-on-linux-x86_64-cloud-vm-4-vcpu-xeon-21-ghz-2026-09-27). sbx served
the first attempt **20/20** on nginx (median 240 ms) and on postgres (348 ms). Lazytainer served
it **0/5** on both (3061 and 3407 ms). Sablier could not be stood up, and zeropod needs a cluster.
That run needed a fix to `compare.sh`'s postgres client on native Linux, described there.

#### Measured · 2026-08-15

Conditions printed by the run, copied from the artifact rather than remembered:
darwin/arm64, **host load 5.37**, 285 MB free in the VM, docker 29.2.1, **noise floor
380 µs/req ±90 µs**. Idle windows differ per arm and print too - sbx 5 s, Sablier 60 s,
Lazytainer 10 s. This is a loaded laptop; the numbers below are a comparison taken under one
set of conditions, not a specification.

| contender | target | n | median | paired delta | **first attempt served** | overhead | resident |
|---|---|---|---|---|---|---|---|
| **sbx** | nginx | 5 | **174 ms** | **116 ms** | **5/5** | 33 µs/req ±21 µs | 13.9 MB `ps` |
| **sbx** | postgres | 5 | 931 ms | 511 ms | **5/5** | n/a | 13.0 MB `ps` |
| Lazytainer | postgres | 5 | 3286 ms | 3198 ms | **0/5** | n/a | 10.2 MB `docker stats` |
| Lazytainer | nginx | - | SKIPPED | - | - | - | could not be stood up on this run |
| Sablier | nginx | - | SKIPPED | - | - | - | middleware did not block: a request to a stopped target failed instead of waiting |
| Sablier | postgres | - | **N/A** | - | - | - | HTTP-only by design - a middleware on an HTTP request cannot wake a `psql` client |
| **zeropod** | nginx | 4 | **272 ms** | - | **4/4** | - | **RAM and processes, via CRIU** - measured in CI, see below |

**The column that matters is "first attempt served", not the milliseconds.**

Lazytainer wakes on a *packet threshold*. Measured directly: attempts 1-5 were refused in
about a millisecond each, and the sixth was served 5150 ms after the first. It never holds
the connection. So a client that does not retry - `psql`, a connection pool, a test runner
somebody else wrote - does not get a slow response from it. It gets a failure.

sbx served the first attempt every time, on both targets. That's the core claim this project
makes about sbx's wake path.

Its 3286 ms is also not a latency to compare with ours: it is gated by its own 3 s poll
rate, which is why its spread is 43 ms against our 19 ms. Different mechanisms, not a
faster or slower version of the same one.

**Overhead: 33 µs/req over a same-container floor, jitter ±21 µs.** It's the same quantity
`proxy_bench_test.go` put at ~15 µs when this run was taken (14 µs since 2026-08-31) by a different method: benchstat times a bare loopback
echo, this times HTTP through a real container, so the two are close rather than equal and
neither replaces the other. Rows without a same-container baseline print `n/a` instead of a
number, since a delta between two separately-run containers isn't a valid comparison.

**Still unmeasured:** Sablier's wake path, because its Traefik middleware would not engage
under any plugin configuration tried; and Lazytainer's nginx arm on this run.

---

### Conditions matter

`scripts/bench.sh` prints host load and VM memory alongside its results, because a wake on
an idle laptop and a wake on a busy one are not the same measurement.

**A worked example.** After hours of end-to-end suites the same machine sat at load average 9
with 30 containers and 484 volumes, and `bench.sh 20` returned median 262 ms, stdev 314 ms -
against the 191 ms / stdev 24 ms above. The 191 ms figure stands, for two reasons:

- **A noisy measurement doesn't refute a clean one.** The stdev under load is thirteen times
  larger; it measures the machine, not the wake.
- **Whether the code changed is a different question with a different answer.** An interleaved
  A/B of the two builds on that same loaded machine (order alternating, n=14) showed no
  difference outside the noise either way. A paired comparison holds up under conditions that
  would make an absolute one unreliable - which is why the harness is built that way.

So: 191 ms stands as measured under the conditions named beside it. v0.14.0 on a Linux cloud VM
measured 216 ms, stdev 14 ms: a different machine, not a refutation.

---

### Engineering notes

How some of the numbers above were found, broken and fixed. Kept for anyone changing these
paths; not needed to read the figures.

#### What the egress activity stamp and the feature gates cost (v0.8.0)

Each of these sits in a hot path, so the question is what it charges when it is doing nothing.

```
  egress filter, per 32 KiB chunk
    without the activity stamp     1.84 ns     0 allocs
    with it                        4.17 ns     0 allocs        +2.33 ns

  feature gate check
    SBX_FEATURES set              97.04 ns     2 allocs
    SBX_FEATURES unset            34.20 ns     1 alloc
```

**The egress stamp is 2.3 ns per 32 KiB and allocates nothing.** It is what lets a box that only
calls out count as busy; the alternative was `idle: "never"`, which holds that box's memory for
the sandbox's whole life. The gate check is off the data path entirely — it runs when a command
starts, not per connection.

The waiting page has no steady-state cost to measure: it is off unless
`SBX_FEATURES=waiting-page`, and even then it does nothing until a wake has already run longer
than a second.

#### Listing sandboxes

`List` is called by the daemon's discovery on every refresh tick, by `AllocSlot` on every
create, and by nine CLI commands - so its cost is paid on a timer, continuously, and grows
with the number of sandboxes on the machine.

The old path ran one `docker ps` plus one `docker inspect` **per container**. Interleaved A/B
of `sbx list` against 13 containers, paired because the runs alternate:

```
  ps + inspect per container   median  330.7 ms   min 233.4   max 1021.2
  one Engine API request       median   78.8 ms   min  56.2   max  514.5

  paired delta                 median +237.6 ms
```

**Four times faster at 13 containers, and O(1) process spawns instead of O(n).** At twenty
sandboxes the old path was spawning dozens of docker CLI processes every fifteen seconds,
contending for the same daemon that wakes are trying to use - so discovery cost landed on the
wake path exactly when the machine was busiest. The Engine API client behind the new path was
already in the repo, written for precisely this.

#### Build cache

`build:` tags an image by a hash of its context, so the question is what a cache hit actually
saves. `sbx create`, wall clock, n=10 each, same machine, interleaved with the baseline:

```
  cold cache (builds)       n=10   median  1070 ms   min  860 ms   max 2133 ms
  warm cache (skipped)      n=10   median   590 ms   min  360 ms   max 2241 ms
  image: (pull, no build)   n=10   median   798 ms   min  493 ms   max 1092 ms
```

**A build costs about 480 ms here; a cache hit is statistically indistinguishable from a plain
image create.** The runs spread 360-2241 ms, wide enough that the warm-cache median landing
slightly below the plain-`image:` baseline isn't a meaningful difference - and that's the claim
worth making anyway: the point of hashing the context is that the second create does no build
work at all, not that it somehow beats pulling.

The 480 ms here is one `RUN echo` on `nginx:alpine`; a real Dockerfile is seconds to minutes,
which is the whole reason the cache key is content and not a clock - a time-based expiry can
rebuild work that hasn't changed or reuse work that has, and either way costs the full build.
