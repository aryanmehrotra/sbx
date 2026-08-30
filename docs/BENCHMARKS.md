# Benchmarks

> **Short version:** wake in 191 ms (redis) to ~1 s (postgres) · a new connection to an awake
> sandbox costs about +0.1 ms · bulk transfer runs at 57% of direct · a sleeping sandbox is 0 B
> of memory.

Every number here was measured on the machine described beside it, by a script in this repo
that you can run. Nothing is quoted from a single run — every figure names the script that
produced it, so you can reproduce it or challenge it.

```sh
scripts/bench.sh 20                                  # wake latency, distribution
go test -run '^$' -bench RoundTrip -count 12 .       # proxy overhead, for benchstat
go test -run '^$' -bench Stream -count 10 ./internal/daemon   # bulk throughput
./sbx selftest                                       # the whole cycle, ~9s
./scripts/e2e.sh 3                                   # several sandboxes at once
./scripts/recovery.sh                                # kill the daemon, twice
./scripts/fork-e2e.sh                                # snapshot, fork twice, prove independence
./scripts/interrupt-e2e.sh                           # kill a volume copy mid-write, source stays intact
./scripts/soak.sh 600                                # endurance: fd/RSS flat under connection churn
scripts/compare.sh 20                                # sbx against the field
```

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

## Wake

| | median | detail |
|---|---|---|
| docker | **191 ms** | n=20, p90 232 ms, stdev 24 ms |
| kubernetes | **1534 ms** | n=5, min 1362, max 2060 - a pod must be scheduled |

**Both assume a declared `health` command.** Without one there is nothing to ask - docker
binds a published port the instant the container starts, so dialling it proves nothing - and
the daemon waits a flat **2 s** before letting the caller through. That is ten times the
number above, on a configuration the spec permits, which is why every bundled template
declares a health check and why SPEC.md calls it close to required.

These are a laptop and a minikube. For scale against hosted platforms, see
[against other platforms](#against-other-platforms) below.

### Why wake is fast

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
   wake     191 ms
   create   492 ms
   cluster 1534 ms
```

---

## Proxy overhead

The wake is a one-off; this is the tax on every query for the life of the sandbox.

```
             │ direct      │ proxied     │
             │ sec/op      │ sec/op      vs base
RoundTrip-10   14.52µ ± 3%   30.10µ ± 8%  +107.32% (p=0.000 n=12)
```

**About 15 µs.** That's +107% against a bare loopback echo, or +7% against a real query that
already crosses a VM boundary at 426 µs - the baseline you pick changes the headline, so both
are given here rather than just the flattering one. Against the workloads sbx actually fronts,
where a query already costs hundreds of microseconds before it reaches the proxy, an extra
15 µs is not something a client will notice.

**This measures round trips on a connection that is already open.** What a client pays to
*open* one is a separate number, covered below.

---

## Throughput

The proxy overhead above is a latency figure on a six-byte PING. The workloads sbx actually
fronts are databases and browsers: a `pg_dump`, a `COPY`, a large result set, a CDP screenshot -
so it's worth knowing what sitting in that path costs on real transfer volumes, not just pings.

`go test -bench Stream -benchtime 30x -count 10`, 16 MiB per iteration, loopback:

```
  direct    n=10   median 12136 MB/s   min 6678   max 12451
  proxied   n=10   median  6870 MB/s   min 5627   max  7643
```

**A bulk transfer runs at about 57% of direct on loopback.** Even at that rate, 6.8 GB/s is an
order of magnitude above what a Postgres `COPY` or similar workload actually produces, so the
database stays the binding constraint, not the proxy. It would only start to matter for
something that genuinely streams at memory speed.

### The relay buffer: pooled, and bigger

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

## A new connection to an awake sandbox

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

**A new connection to an already-awake sandbox costs about +0.1 ms** over dialling docker
directly. The daemon treats a unit it woke and hasn't slept as awake and serves the connection
without re-checking the workload's health on every dial - and that belief is verified
optimistically: if the container was stopped from outside, the upstream connect fails, the
belief is revoked, and a proper wake runs. Both paths are covered by tests in
`internal/daemon/awake_test.go`.

---

## A heavier workload: headless Chrome

Redis is the wake benchmark because it isolates the wake path from the workload's own
startup. Chrome is the other end of the range - the browser template, woken by a plain CDP
request:

```
  cold   run 1  4356 ms    run 2  3744 ms    run 3  3030 ms
  warm   run 4   703 ms    run 5   829 ms
```

Cold median **3744 ms**, warm median **766 ms** (n=5, macOS arm64). Layer and page-cache
warming bring later runs down within a session, so the number to plan around is two regimes:
seconds on first touch, well under a second once the image is warm. The cost here is Chrome's
own startup - the same image started by hand costs the same - which is the point: sbx removes
the cost of a browser nobody is using, not the cost of starting one.

---

## Listing sandboxes

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

---

## Build cache

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

---

## Memory

Both containers fresh, both idle, same image - which is the only comparison that means
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

## Against other platforms

Vendor-documented figures, read August 2026, beside ours - useful context, not a controlled
benchmark. The differences below explain why.

| | idle → serving | what comes back | measured by |
|---|---|---|---|
| **sbx** docker | **191 ms** | disk warm, process cold | `scripts/bench.sh 20`, this repo |
| **sbx** kubernetes | **1534 ms** | disk warm, process cold | `scripts/bench.sh`, minikube |
| E2B resume | ~1000 ms | **RAM + processes** | [vendor docs][e2b] |
| Neon | a few hundred ms | Postgres data | [vendor docs][neon] |
| Fly, suspended | a few hundred ms | RAM snapshot | [vendor docs][fly] |
| Fly, stopped | ~2000 ms+ | disk | [vendor docs][fly] |
| Daytona | *none published* | disk, persistent volume | - |
| Knative | pod schedule, seconds | volume if attached | - |

[e2b]: https://docs.e2b.dev/sandbox/persistence
[neon]: https://neon.com/docs/connect/connection-latency
[fly]: https://fly.io/docs/reference/suspend-resume/

### Why these numbers don't belong in the same table

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

## Against the field, measured here

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
| overhead is measured against **the same container without the wake path**, interleaved | measuring all the floor then all the through path lets load drift land in the answer - this floor moved 660 µs → 4280 µs between two runs on one machine |
| a delta inside the harness's own jitter is **not published as a number** | the jitter here is ±150-900 µs and the proxy tax is ~15 µs, so this harness cannot resolve it and says so |

`N/A` and `SKIPPED` are different facts. Sablier has no postgres row because it is HTTP-only
by design - that is a *result*.

Correctly gating zeropod required scraping its `zeropod_running` metric directly (0 when
checkpointed), since the pod stays `Running` while checkpointed and `kubectl get pod` alone
can't tell asleep from awake. `scripts/zeropod-probe.sh` does that scrape from inside the
cluster, which is what produced the 272 ms row below.

### Measured · 2026-08-15

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
`proxy_bench_test.go` puts at ~15 µs by a different method: benchstat times a bare loopback
echo, this times HTTP through a real container, so the two are close rather than equal and
neither replaces the other. Rows without a same-container baseline print `n/a` instead of a
number, since a delta between two separately-run containers isn't a valid comparison.

**Still unmeasured:** Sablier's wake path, because its Traefik middleware would not engage
under any plugin configuration tried; and Lazytainer's nginx arm on this run.

---

## Conditions matter

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

So: 191 ms stands as measured under the conditions named beside it.
