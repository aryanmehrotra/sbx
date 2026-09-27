# Benchmarks

Every performance figure sbx publishes, with the script, machine and version behind it. Other pages
link here instead of repeating the numbers.

## Headline numbers

Measured on v0.14.0 on 2026-09-27, on the [cloud VM](#the-v0140-machine) unless the row says
otherwise. Older figures from other machines are in [earlier runs](#earlier-runs). A figure from a
different machine is not a speed-up or a regression.

| Figure | What | Version · machine | Script |
|---|---|---|---|
| 20/20 vs 0/5 | first connection served on wake, postgres: sbx vs Lazytainer | v0.14.0 · cloud VM | `scripts/compare.sh` |
| 0 B | memory of a sleeping sandbox (no container running, 3/3 rounds) | v0.14.0 · cloud VM | `scripts/bench-memory.sh` |
| 12.8 MB | daemon RSS at rest, no sandboxes (12.6-12.9, n=3) | v0.14.0 · cloud VM | `scripts/bench-memory.sh` |
| 17.6 MB vs 629 MB | 20 idle postgres databases: sbx asleep (daemon only) vs 20 always-on containers (2 runs: 17.4/17.6 vs 667/629) | v0.14.0 · cloud VM | `scripts/bench-fleet-memory.sh 20` |
| 216 ms | docker wake, redis (median, n=20, p90 236, stdev 14) | v0.14.0 · cloud VM | `scripts/bench.sh 20` |
| 34 ms | docker thaw from `"on_idle": "freeze"`, redis (n=20, p90 37); 12 ms over the same ping awake | v0.14.0 · cloud VM | `scripts/bench-freeze.sh 20` |
| 348 / 240 ms | docker wake, postgres (p90 488) / nginx (p90 261), n=20 each | v0.14.0 · cloud VM | `scripts/compare.sh 20` |
| 611 / 387 ms | headless Chrome wake, cold / warm, n=5 each | v0.14.0 · cloud VM | `scripts/bench-chrome.sh` |
| 362 ms | `sbx create`, redis, image present (n=10, p90 375) | v0.14.0 · cloud VM | `scripts/bench-create.sh` |
| 12.8 ms | OpenSandbox create → first command, docker warm pool, one at a time (n=10, p95 77.8) | v0.14.0 · cloud VM | `scripts/osb-bench.sh` |
| 309-460 ms | the same, 100 at once from the pool (two runs, n=100 each) | v0.14.0 · cloud VM | `scripts/osb-bench.sh --burst 100` |
| 144 ms | OpenSandbox create → first command, frozen microVM pool, 4 at once | v0.14.0 · GitHub `ubuntu-24.04`, nested KVM | CI `microvm` job |
| +0.17 ms | a new connection to an awake sandbox, over docker direct | v0.14.0 · cloud VM | `scripts/connbench.sh` |
| +9.6 µs | round trip on an open connection (9.4 → 19.1 µs, n=12) | v0.14.0 · cloud VM | `go test -bench RoundTrip` |
| +87 µs | opening a connection, in-process (127 → 214 µs, n=10) | v0.14.0 · cloud VM | `go test -bench Conn` |
| 1.39 GB/s | bulk throughput through the proxy, 55% of direct (2.54 GB/s), n=10 | v0.14.0 · cloud VM | `go test -bench Stream` |
| 1534 ms | kubernetes wake (n=5); stale, the wake path changed in v0.13 | v0.1.0 · minikube | `scripts/bench.sh` |
| 216 ms | microVM wake from a Mac through the helper VM; stale, before the jailer and layered root | v0.11.0 tree · Apple M4, colima | `scripts/fc-anywhere-e2e.sh` |

Not measured anywhere yet: a microVM wake on bare-metal Linux, snapshot disk use per VM, a
Windows/WSL2 host, kata-fc on kubernetes, and concurrent microVM restores off nested
virtualisation. The [Firecracker spike](design/2026-09-26-firecracker-spike.md) projects 4-28 ms
for a bare-metal wake from published restore figures; only a bare-metal run can confirm it.

## The v0.14.0 machine

| | |
|---|---|
| tree | v0.14.0 code, `8dff521` (later commits are docs only), `go build -o sbx .` |
| machine | cloud VM, 4 vCPU `Intel(R) Xeon(R) Processor @ 2.10GHz`, 15 GiB RAM, no swap |
| kernel | `6.18.44-fc-v37` |
| docker | 29.3.1, `overlayfs`, cgroup driver `cgroupfs`, cgroup v1 |
| Go | go1.26.0 linux/amd64 |
| not available | `/dev/kvm` (no microVM runs), kubernetes |

Scripts ran one after another, never at once. Images were pulled first, so no timing includes a
download. Host load was 0.1-1.9 at the start of each script.

This is a VM, not bare metal, and noisier than a laptop. Its cores are slower than an M4's, so the
loopback benchmarks are slower in absolute terms while their ratios to direct stay close. Wake
figures include each harness's client start: about 12 ms of timer start in `bench.sh`, plus
`redis-cli`'s.

## Wake, redis

`scripts/bench.sh 20` sleeps a redis sandbox, then times `redis-cli` against it.

- Median 216 ms, min 191, p90 236, max 251, stdev 14 ms, 20/20 served.
- The timer's own start costs 11-13 ms per sample here.
- Both this and the kubernetes figure assume the spec declares a `health` command. Without one,
  the daemon waits a flat 2 s per wake, because a published port is bound before the service is
  ready.

```sh
scripts/bench.sh 20
```

## Freeze and thaw (v0.14.0)

The redis spec with `"on_idle": "freeze"`, so going idle runs `docker pause` instead of a stop.
Each run waits until docker reports the container paused, then times one `redis-cli ping`.

- Thaw: median 34 ms, p90 37, min 28, max 40, 20/20 served.
- The same ping with the sandbox awake: median 20 ms (17-27). That floor sits inside every thaw.
- Paired per run, the thaw costs 12 ms over it (p90 17, min 6, max 19).

```sh
scripts/bench-freeze.sh 20
```

## Wake against other tools

`scripts/compare.sh` runs sbx and self-hosted rivals against the same targets on one machine. A
sample counts only on a correct protocol reply, from a target verified asleep, paired with a
baseline through the same client. A delta inside the harness's jitter is not published.

| Contender | Target | n | Median | p90 / max | Paired delta | First attempt served |
|---|---|---|---|---|---|---|
| sbx | nginx | 20 | 240 ms | p90 261 | 216 ms | 20/20 |
| sbx | postgres | 20 | 348 ms | p90 488, stdev 74 | 236 ms | 20/20 |
| Lazytainer | nginx | 5 | 3061 ms | max 4074 | 3037 ms | 0/5 |
| Lazytainer | postgres | 5 | 3407 ms | max 5585 | 3303 ms | 0/5 |
| Sablier | nginx | - | skipped | - | - | could not be stood up |
| Sablier | postgres | - | N/A | - | - | HTTP-only by design |
| zeropod | both | - | omitted | - | - | no kubernetes on this machine |

- The first-attempt column matters more than the milliseconds. Lazytainer wakes on a packet
  threshold and refuses the first attempts, so a client that does not retry gets a failure.
- Noise floor 215 µs/req ±99. sbx's per-request overhead on nginx, 134 µs ±34, is inside it, so
  it is not a result.
- zeropod was measured in CI at 272 ms median, n=4, 4/4 served ([earlier runs](#earlier-runs)).

```sh
CONTENDERS=sbx scripts/compare.sh 20
CONTENDERS=lazytainer,sablier,zeropod scripts/compare.sh 5
```

## A heavier workload: headless Chrome

`examples/browser` with `sbx serve --idle 5s`, woken by `curl /json/version`, 10 runs alternating.

- Cold, page cache dropped first: median 611 ms (607-619).
- Warm: median 387 ms (368-413).
- The same `curl` against the awake browser takes 18-20 ms.
- Most of the wake is Chrome's own startup. "Cold" here is not the same condition as the v0.1.0
  first touch on a Mac (3744 ms).

```sh
scripts/bench-chrome.sh
```

## Create

`sbx create` of `bench.sh`'s redis spec, then `sbx rm`, 10 times. Median 362 ms, p90 375, min
345, max 391. That includes 11-13 ms of timer overhead, which the script prints.

```sh
scripts/bench-create.sh 10
```

## Memory

Three rounds, each with a fresh `sbx serve --idle 5s`, measured with `ps -o rss`.

| State | RSS |
|---|---|
| daemon at rest, no sandboxes | 12.8, 12.9, 12.6 MB |
| fronting one sleeping redis sandbox | 13.1, 13.4, 13.0 MB |
| after a wake, 1000 pipelined PINGs and 50 new connections | 13.4, 13.7, 13.2 MB |
| the sleeping sandbox itself | 0 B: no container running, every round |

At the end of the 20-run `compare.sh` arms the daemon was 16.4 MB. A Linux amd64 binary is not the
macOS arm64 one, so 12.8 MB against v0.1.0's 9.1 MB is not growth.

```sh
scripts/bench-memory.sh
```

### Twenty idle databases

`scripts/bench-fleet-memory.sh 20` starts 20 containers of the postgres template's image with
its environment, the way docker compose would, waits until each accepts connections plus 30 s,
and sums `docker stats`. Then it creates 20 sbx sandboxes from `--template postgres`, waits until
the daemon has put every one to sleep, and reads the daemon's RSS.

| Run | 20 always-on containers | 20 sbx sandboxes, asleep |
|---|---|---|
| 1 | 666.7 MB | 17.4 MB (0 containers running) |
| 2 | 629.4 MB | 17.6 MB (0 containers running) |

The README chart uses run 2, the smaller always-on figure. An idle Postgres holds about 31-33 MB;
a sleeping sandbox holds none, and the daemon grows by under 5 MB for twenty of them.

## A new connection to an awake sandbox

Clients without a pool (`psql`, `redis-cli`) open a connection per operation, so this is the cost
they pay every time. Interleaved against the same awake container, n=20 per side.

- Three runs: +0.15 ms (IQR +0.10 to +0.22), +0.18 ms (+0.08 to +0.30), +0.17 ms (+0.06 to +0.25).
- The daemon trusts a sandbox it woke as awake, and re-checks only if the upstream connect fails
  (`internal/daemon/awake_test.go`).

```sh
scripts/connbench.sh 20
```

## Proxy overhead

In-process, on loopback, with no container, read with `benchstat`:

```
  RoundTrip  direct   9.43 µs ±3%   proxied  19.08 µs ±1%   +9.6 µs  (+102%)   -count 12
  Conn       direct 127.1  µs ±5%   proxied 214.3  µs ±4%   +87 µs   (+69%)    -count 10
  Stream     direct 2543 MB/s ±7%   proxied 1391 MB/s ±13%  55% of direct      -benchtime 30x -count 10
```

- RoundTrip is the cost per query on an open connection. On an M4 it was +114% over a bare
  loopback echo and +7% over a query that takes 426 µs.
- Conn is the cost of opening one, which pool-less clients pay on every operation.
- Stream is 16 MiB per iteration. Even at half of direct, the database stays the bottleneck.

```sh
go test -run '^$' -bench RoundTrip -count 12 ./internal/daemon
go test -run '^$' -bench Conn -count 10 ./internal/daemon
go test -run '^$' -bench Stream -benchtime 30x -count 10 ./internal/daemon
```

### Relay buffer

Each direction of a tunnel copies through a pooled 64 KiB buffer: 8 ns and 0 allocations per
connection, against 5995 ns and 64 KiB allocated before (`go test -bench RelayBufAcquire
-benchmem`). A larger buffer is faster on an Apple M4 (`-count 6`, MB/s):

```
  32 KiB 4440 · 64 KiB 4920 (ships) · 128 KiB 5150 · 256 KiB 6800 · 512 KiB 7010 · 1 MiB 6870
```

64 KiB ships because the pool holds two buffers per live connection. At 60 concurrent streams the
daemon peaked at 23 MiB with 64 KiB and 33 MiB with 256 KiB. The curve is machine-specific, so
re-measure before changing `relayBuf`.

## OpenSandbox create → first command

ComputeSDK's TTI shape ([computesdk/benchmarks](https://github.com/computesdk/benchmarks),
METHODOLOGY.md): client-timed `create()` to the first successful `runCommand('node -v')`, through
the upstream OpenSandbox Go SDK, `node:22-slim`. This is a local number, not a leaderboard entry:
ComputeSDK measures hosted endpoints over the internet.

| Run | Mode | n | ok | Median ms | p95 ms | p99 ms | Score |
|---|---|---:|---:|---:|---:|---:|---:|
| burst 1 × 10, interleaved | pool | 10 | 10 | 12.8 | 77.8 | 77.8 | 99.62 |
| burst 1 × 10, interleaved | cold | 10 | 10 | 341.8 | 392.1 | 392.1 | 96.39 |
| burst 100, run 1 | pool | 100 | 100 | 309.2 | 404.7 | 405.8 | 96.53 |
| burst 100, run 2 | cold | 100 | 100 | 8970.5 | 17710.5 | 17717.3 | 6.18 |
| burst 100, run 3 | pool | 100 | 100 | 459.6 | 599.5 | 605.8 | 94.84 |

- The two pool bursts differ by 150 ms, so a pool burst of 100 here is 300-460 ms.
- A cold burst of 100 is "seconds to tens of seconds"; runs differ too much to say more.
- Every claim re-keys execd, the agent inside the sandbox. At 100 at once that costs ~40-60 ms
  of median, one round trip per claim; one at a time it is inside the spread.
- `SBX_OSB_TRACE=1` logs each phase of a create, which is how the phase figures in
  [earlier runs](#earlier-runs) were read.

```sh
scripts/osb-bench.sh --burst 1 --rounds 10 --pool node:22-slim=8 --burst-modes default,cold
scripts/osb-bench.sh --burst 100 --rounds 1 --pool node:22-slim=100
scripts/osb-bench.sh --burst 100 --rounds 1 --burst-modes cold
```

## OpenSandbox on microVMs

The same shape with every sandbox a Firecracker microVM, run by CI's `microvm` job on a GitHub
`ubuntu-24.04` runner with nested KVM and the jailer on. These numbers compare one commit with
another; no bare-metal run exists.

| Mode (3 rounds × 4 at once) | v0.14.0 median | v0.13.0 median (`e0749be`) | v0.13.0 p95 |
|---|---:|---:|---:|
| cold, no pool | 2,637 ms | 2,821 ms | 5,769 ms |
| pool, members asleep | 759 ms | 699 ms | 957 ms |
| pool, members frozen | 144 ms | 141 ms | 207 ms |

- Cold spends ~2.6 s in `create()`, then runs `node -v` in ~65 ms.
- An asleep member is a snapshot load: `create()` returns in ~260 ms, but memory pages in lazily,
  so the first `node -v` takes 320-640 ms.
- A frozen member is paused in RAM: `create()` is a ~37 ms re-key. It holds its RAM while waiting.
- The script prints `creates answered from the warm pool: N` but does not fail on zero, so check
  it before trusting a row. v0.13.0 run:
  [36251708713](https://github.com/aryanmehrotra/sbx/actions/runs/36251708713).

```sh
scripts/osb-bench.sh --provider firecracker --burst 4 --rounds 3 --pool node:22-slim=4 \
  --burst-modes default,cold          # add --pool-freeze for frozen members
```

## A microVM on a Mac, through the helper VM

Stale: measured on the v0.11.0 tree (`b21492f`, 2026-09-26), before the jailer (v0.13) and the
layered root disk (v0.14). The create figure will not reproduce, because v0.14 replaced its 2.3 s
root filesystem clone with a hard link.

Apple M4, macOS 26.4.1; colima 0.10.1 helper VM with nested virtualisation, 2 vCPU / 2 GiB;
Firecracker v1.17.0; `nginx` template, 1 vCPU / 256 MiB guest; 12 rounds, timed on the Mac.

| | n | Median | p95 |
|---|---:|---:|---:|
| wake from snapshot → first byte | 12 | 216 ms | 256 ms |
| request to an awake sandbox → first byte | 12 | 8.3 ms | 9.5 ms |
| `sbx exec` round trip (ssh + sbx + execd over vsock) | 12 | 725 ms | 908 ms |
| `sbx sleep` (Seal, pause, Diff snapshot, merge) | 12 | 680 ms | 811 ms |
| `sbx create` (image pulled) | 12 | 11.5 s | 14.3 s |

Inside the VM the daemon's own wake was 206 ms, most of it the 170 ms execd re-key over vsock.
The Mac adds little.

```sh
go build -o sbx . && SBX_FC_VM_DRIVER=colima FC_E2E_ROUNDS=12 scripts/fc-anywhere-e2e.sh
```

## What CI measures

Every tag attaches a `bench.md` to its GitHub release: `RoundTrip` and `Stream`, ten runs each, with
the runner's CPU, memory, kernel and Go version. It is for comparing one release with the last on
the same kind of runner. It does not feed this page, because a shared runner is not a machine you
can describe. Wake latency is left out: it is too noisy on a shared runner.

## Earlier runs

Kept with their machines. A different machine is not a regression or a speed-up.

| What | Figure | Version · machine · date | Script |
|---|---|---|---|
| docker wake, redis | 191 ms (n=20, p90 232, stdev 24) | v0.1.0 · laptop · 2026-08-15 | `scripts/bench.sh 20` |
| docker wake, redis, machine at load 9 | 262 ms (stdev 314); an interleaved A/B showed no code change | the 191 ms laptop, after hours of e2e suites | `scripts/bench.sh 20` |
| docker wake, postgres / nginx | 931 / 174 ms, n=5 | v0.1.0 · darwin/arm64, load 5.37 | `scripts/compare.sh` |
| first attempt served, sbx vs Lazytainer, postgres | 5/5 vs 0/5 (Lazytainer 3286 ms) | v0.1.0 · darwin/arm64, load 5.37 · 2026-08-15 | `scripts/compare.sh` |
| sbx overhead over the same container, nginx | 33 µs/req ±21 | v0.1.0 · darwin/arm64 | `scripts/compare.sh` |
| zeropod wake, nginx | 272 ms, n=4, 4/4 served | CI, kubernetes | `scripts/zeropod-probe.sh` |
| kubernetes wake | 1534 ms, n=5, min 1362, max 2060 | v0.1.0 · minikube · 2026-08-15 | `scripts/bench.sh` |
| headless Chrome, cold / warm | 3744 / 766 ms, n=5 | v0.1.0 · macOS arm64 · 2026-08-16 | none recorded |
| `sbx create`, redis | 492 ms, n=1 | v0.1.0 · laptop | - |
| daemon RSS at rest / fronting one / after traffic | 9.1 / 9.6 / 10.4 MB | v0.1.0 · macOS laptop · 2026-08-15 | `ps -o rss` |
| idle `mysql:8.0`, stock / tuned | 411 / 110 MB | v0.1.0 · macOS laptop · 2026-08-15 | - |
| idle `clickhouse:24.3`, stock / tuned | 199 / 201 MB | v0.1.0 · macOS laptop · 2026-08-15 | - |
| new connection to an awake sandbox | +0.10 ms (IQR -0.03 to +0.21) | v0.1.0 · laptop | `scripts/connbench.sh` |
| round trip on an open connection | +14 µs (12.52 → 26.78 µs) | v0.8.0 · Apple M4 · 2026-08-31 | `go test -bench RoundTrip` |
| opening a connection, in-process | +53 µs (58.23 → 111.00 µs) | v0.8.0 · Apple M4 · 2026-08-31 | `go test -bench Conn` |
| bulk throughput through the proxy | 7.0 GB/s, 56% of direct (12.4 GB/s) | v0.8.0 · Apple M4 · 2026-08-31 | `go test -bench Stream` |
| bulk throughput through the proxy | 6.8 GB/s, 57% of direct, n=10 | v0.7.0 · Apple M4 · 2026-08-21 | `go test -bench Stream` |
| OpenSandbox pool, one at a time | 11.1 ms (13.7 ms with a re-key every claim) | v0.10.0 · Apple M4, colima 3 vCPU · 2026-09-26 | `scripts/osb-bench.sh` |
| OpenSandbox pool, 100 at once | 412.6 / 432.3 ms (472.1 ms with a re-key every claim) | v0.10.0 · Apple M4, colima 3 vCPU | `scripts/osb-bench.sh --burst 100` |
| OpenSandbox cold, one at a time | 227 ms, n=10 (4039 ms before pulls were skipped and create waited for Running) | v0.10.0 · Apple M4, colima 3 vCPU | `scripts/osb-bench.sh` |
| `sbx list`, 13 containers | 78.8 ms (330.7 ms with `docker inspect` per container) | not recorded | interleaved A/B |
| `build:` create, cold / cached / plain `image:` | 1070 / 590 / 798 ms, n=10 | not recorded | interleaved A/B |
| egress filter activity stamp | +2.33 ns per 32 KiB chunk, 0 allocs | v0.8.0 | `go test -bench` |
