# Benchmarks

> **Short version:** wake 191 ms (redis) to ~1 s (postgres) · a new connection costs nothing
> measurable · bulk transfer runs at 57% of direct · a sleeping sandbox is 0 B of memory.
> Every number names the script that produced it, and the ones this project got wrong are
> [listed as corrections](COMPARISON.md#what-we-have-actually-measured-and-what-we-have-only-read).

Every number here was measured on the machine described beside it, by a script in this repo
that you can run. Nothing is quoted from a single run.

```sh
scripts/bench.sh 20                                  # wake latency, distribution
go test -run '^$' -bench RoundTrip -count 12 .       # proxy overhead, for benchstat
go test -run '^$' -bench Stream -count 10 ./internal/daemon   # bulk throughput
./sbx selftest                                       # the whole cycle, ~9s
./scripts/e2e.sh 3                                   # several sandboxes at once
./scripts/recovery.sh                                # kill the daemon, twice
./scripts/fork-e2e.sh                                # snapshot, fork twice, prove independence
scripts/compare.sh 20                                # sbx against the field
```

## What the pipeline measures, and what it does not

Every tag attaches a `bench.md` to its GitHub release: `RoundTrip` and `Stream`, ten runs each,
with the runner's cpu, memory, kernel and Go version written at the top.

**It does not touch the numbers on this page.** A GitHub runner is a shared, virtualised machine
whose neighbours are invisible; its figures are not comparable to a laptop's, and writing them
in here would replace measurements somebody took and can describe with numbers nobody can
attribute - while the text above still said they came from a MacBook. The attached file is for
comparing one release against the release before it, on the same shape of machine, which is the
question a benchmark in a pipeline can actually answer.

Wake latency is deliberately not in it. It needs containers and is dominated by whatever else
the runner is doing, and a number that noisy published on every tag teaches people to ignore the
file it is in. `scripts/bench.sh` is how that one is measured, on a machine you can describe.

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
[against other platforms](#against-other-platforms) below - including why that comparison is
weaker than it looks.

### One thing removed, and one thing tried and refuted

Two changes were made to the wake path after the per-connection fix. Only one of them is real,
and the way the other one failed is worth more than the one that worked.

**Removed: a probe that could only fail.** The wake asked the workload whether it was serving
*before* starting it, to catch a container somebody had started outside sbx. But the unit being
asleep is the reason wake was called, so on its actual path that probe could only fail - and
starting an already-running container is a 304 the provider already treats as success, so it
answered a question `Start` answers for free. One round trip per cold wake, gone. Held by a
test that counts probes rather than timing them: a cold wake costs exactly one.

**Refuted: backing the poll off from 5 ms.** The argument is good on paper - a flat 100 ms
interval rounds a fast workload up to the interval - and a counting test agreed emphatically:
an 8 ms workload was declared awake in 102 ms flat and about 20 ms backing off. It is worth
nothing:

```
  100 ms flat   n=14   median 162 ms   min 143   max 264
  5 ms backoff  n=14   median 166 ms   min 128   max 295
```

The counting test was measuring a model, not the system. It treats a probe as free; a probe is
an Engine API exec, so polling at 5 ms cannot sample sooner than the probe itself costs, and a
real workload is not ready in 8 ms anyway. The interval stayed at 100 ms.

**And the harness had a bias that reversed the sign.** Running A then B in every round hands
B a docker daemon that A has just finished hammering. Under that harness the change looked
8-12% *slower*; with the order alternating it is +3%, which is to say nothing. Both readings
were noise, but only one of them looked like a result.

### How it got there

The first honest measurement was **5282 ms, stdev 22 ms**, and the consistency was the
finding - 0.4% spread is not variable work, it is one fixed cost:

```
   docker start           110 ms   ← redis is already serving here
   State.Health=healthy  +5030 ms  ← docker republishes on its check interval
```

98% of a wake was spent waiting for the platform to notice something that had already
happened. Running the declared health command directly sees it in ~150 ms.

```
   wake     5282 ms → 191 ms
   create   5769 ms → 492 ms
   cluster 18200 ms → 1534 ms
```

---

## Proxy overhead

The wake is a one-off; this is the tax on every query for the life of the sandbox.

```
             │ direct      │ proxied     │
             │ sec/op      │ sec/op      vs base
RoundTrip-10   14.52µ ± 3%   30.10µ ± 8%  +107.32% (p=0.000 n=12)
```

**About 15 µs, which is +107% or +7% depending entirely on the baseline.** Against a bare
loopback echo it doubles the round trip. Against a real query that already crosses a VM
boundary at 426 µs it disappears. Quoting only the flattering number was a mistake this
table exists to prevent.

**This measures round trips on a connection that is already open.** It is not what a
client pays to *open* one. That is the next section, and for a long time it was the missing
number.

---

## Throughput

The proxy overhead above is a latency figure on a six-byte PING, and until now every
benchmark and script in this repo moved six bytes. The workloads sbx actually fronts are
databases and browsers: a `pg_dump`, a `COPY`, a large result set, a CDP screenshot. Whether
sitting in that path cost 5% or 300% was simply unknown - the same position this project was
in about the per-connection cost before somebody measured it.

`go test -bench Stream -benchtime 30x -count 10`, 16 MiB per iteration, loopback:

```
  direct    n=10   median 12136 MB/s   min 6678   max 12451
  proxied   n=10   median  6870 MB/s   min 5627   max  7643
```

**A bulk transfer runs at about 57% of direct - call it a 43% cost.** That is a real number
and it is published because it is real, not because it is flattering. It is also, in
practice, far above what the workloads behind it produce: 6.8 GB/s on loopback is an order of
magnitude more than a Postgres `COPY` will feed you, so the binding constraint stays the
database. It would matter for something that genuinely streams at memory speed.

**A bigger buffer was tried and is worse**, which is the opposite of the obvious fix. Each
direction copies through a 32 KiB buffer, and the reasonable suggestion is that fewer, larger
syscalls would be cheaper:

```
   32 KiB   5274 MB/s      (what it does)
   64 KiB   5541 MB/s      within the noise
  256 KiB   2654 MB/s      half the throughput
```

So it stays at 32 KiB. On Linux there is a real avenue this does not take - `io.Copy` between
two `*net.TCPConn` can reach `splice(2)` and avoid the userspace copy entirely - but the
per-chunk `touch()` that records activity is exactly what defeats the type assertion
`splice` depends on, and these numbers are from macOS, where `splice` does not exist. Worth
revisiting with a Linux measurement; not worth guessing at.

---

## A new connection to an awake sandbox

Two numbers were measured here for a long time - the wake, and the round-trip tax above -
and between them sat the case that turns out to be the common one. A client that opens a
connection per operation (`psql`, `redis-cli`, any CLI, anything without a pool) pays
neither: the sandbox is already awake, so there is no wake, and every operation is a new
connection, so the round-trip figure does not describe it.

`scripts/connbench.sh`, interleaved against the same awake container, n=20 each side:

```
  through the daemon     median   0.79 ms   min 0.67   max 5.60
  straight to docker     median   0.69 ms   min 0.56   max 1.47

  per-connection cost   median +0.10 ms   IQR [-0.03, +0.21]   slower in 13/20 pairs
```

**The first version of this gate was wrong, and in the flattering direction.** It compared
a difference of *medians* against `max - min` of the raw samples - a threshold set by whichever
single outlier was worst, which any small delta passes automatically. It then reported
"indistinguishable from zero" about the number that verifies this project's own headline fix.

The samples are collected in interleaved pairs, so the pairing was there and thrown away.

`connbench.sh` now subtracts pair by pair, as `measure.sh` has always required, and reports the
median delta with its IQR.

The honest reading: on a quiet machine the cost is a fraction of a millisecond and the IQR
straddles zero, so it is not resolvable. What *is* claimed is that it is not 68 ms.

**It was 68 ms.** The daemon re-ran the health command through `docker exec` on every
accepted connection - including connections to a sandbox it had woken minutes earlier and
never slept. So the honest reading of this project's own published overhead, before the fix,
was: 33 µs if you hold one connection open, and 68 ms per operation if you do not.

```
  before   through the daemon   median  68.47 ms   (straight to docker 0.79 ms)
  after    through the daemon   median   0.79 ms   (straight to docker 0.99 ms)
```

The fix is one branch: a unit the daemon woke and has not slept is awake, and asking the
workload again tells it nothing it does not know. It is optimistic, so it is corrected rather
than trusted - if the container was stopped from outside, the upstream dial fails, the belief
is revoked and the wake runs properly. Both halves are held by tests in
`internal/daemon/awake_test.go`, and both were confirmed by deleting the code and watching
them fail.

---

## A heavier workload: headless Chrome

Redis is the wake benchmark because it isolates the wake path from the workload's own
startup. That makes it the right measurement and a misleading one to generalise from, so
here is the other end of the range - the browser template, woken by a plain CDP request:

```
  cold   run 1  4356 ms    run 2  3744 ms    run 3  3030 ms
  warm   run 4   703 ms    run 5   829 ms
```

Cold ≈ 4356/3744/3030, median **3744 ms**; warm 703/829, median **766 ms**.

**Two regimes, not one number.** Reporting `median 3030 ms` over that series was wrong in a
quieter way than the 624 ms it replaced: the samples fall monotonically until they plateau,
which is layer and page-cache warming, and a median over a warming series is a statistic of
nothing. Cold is about 4.4 s, warm about 0.75 s, n=5, macOS arm64. The honest summary is that
a browser costs seconds on first touch and well under a second thereafter - which is better
than the single number suggested, and is the shape a caller should plan for.

**Neither is the 624 ms this project's own use-case doc claimed** before anybody ran it. That number had no script behind it and was wrong by a factor of five; it is the reason
every figure in these docs now names the script that produced it.

The spread is real and wide, and the wake is Chrome's own startup rather than sbx's - the
same image started by hand costs the same. Which is the honest framing for the whole feature:
sbx removes the cost of a browser nobody is using, not the cost of starting one.

---

## Listing sandboxes

Not a user-facing number, and that is exactly why it went unmeasured for so long. `List` is
called by the daemon's discovery on every refresh tick, by `AllocSlot` on every create, and by
nine CLI commands - so its cost is paid on a timer, for ever, and grows with the number of
sandboxes on the machine.

It ran one `docker ps` plus one `docker inspect` **per container**. Interleaved A/B of `sbx
list` against 13 containers, paired because the runs alternate:

```
  ps + inspect per container   median  330.7 ms   min 233.4   max 1021.2
  one Engine API request       median   78.8 ms   min  56.2   max  514.5

  paired delta                 median +237.6 ms
```

**Four times faster at 13 containers, and O(1) process spawns instead of O(n).** At twenty
sandboxes the old path was spawning dozens of docker CLI processes every fifteen seconds,
contending for the same daemon that wakes are trying to use - so discovery cost landed on the
wake path exactly when the machine was busiest.

The Engine API client this now uses was already in the repo, written for precisely this, with
no callers.

---

## Build cache

`build:` tags an image by a hash of its context, so the question is what a cache hit actually
saves. `sbx create`, wall clock, n=10 each, same machine, interleaved with the baseline:

```
  cold cache (builds)       n=10   median  1070 ms   min  860 ms   max 2133 ms
  warm cache (skipped)      n=10   median   590 ms   min  360 ms   max 2241 ms
  image: (pull, no build)   n=10   median   798 ms   min  493 ms   max 1092 ms
```

**A build costs about 480 ms here; a cache hit costs nothing measurable.** The warm median
came out 208 ms *below* the plain-`image:` baseline, which is not a result - the runs spread
360-2241 ms, so a 208 ms difference is well inside the jitter and the honest statement is
that a cache hit and a plain image create are indistinguishable. That is the claim worth
making anyway: the point of hashing the context is that the second create does no build work
at all, not that it somehow beats pulling.

The 480 ms is this Dockerfile - one `RUN echo` on `nginx:alpine`. A real one is seconds to
minutes, which is the whole reason the cache key is content and not a clock: Daytona's
24-hour expiry rebuilds work that has not changed and reuses work that has, and both errors
cost the full build.

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

**Corrected 2026-08-15: the daemon was published at 4.5 MB and that figure is wrong.**
Measured by `ps -o rss` on this build: 9296 KiB with no sandboxes at all, 9808 KiB fronting
one, 10640 KiB after a wake and some traffic. It is not a matter of different quantities -
the at-rest number, the most favourable one available, is still twice what was claimed.

What the new figures do show is that the growth is small and bounded: about half a megabyte
to front a sandbox, and the rest is buffers that traffic touches. The 4.5 MB claim was in
the README, ARCHITECTURE's diagram and this table, and is corrected in all three.

MySQL's saving is real and comes from `performance_schema=OFF` and a 48 MB buffer pool.

**ClickHouse's does not exist**, and an earlier version of this file claimed 1198 MB → 189 MB.
That compared a loaded server carrying real data against a fresh empty one and credited the
difference to configuration. An idle ClickHouse is about 200 MB either way; the cache caps
matter under load, not at rest. The same mistake inflated the MySQL figure from 3.7× to 6×.

---

## Against other platforms

Vendor-documented figures, read August 2026, beside ours. **This is not a benchmark**, and the
next section explains why it would be dishonest to present it as one.

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

They are here because people ask, and refusing to answer is its own kind of dishonesty. But
four things make the column non-comparable, and all four favour us:

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
number, and each exists because of a specific way this kind of benchmark lies:

| rule | why |
|---|---|
| a sample counts only on a **correct protocol reply** | Sablier's middleware failed to engage during development and returned **502 in 98 ms** - faster than sbx's real wake. A status code is not evidence |
| a sample is **VOID** unless the target was verifiably asleep at `t0` | otherwise a rival whose mechanism never engaged scores a spectacular wake for answering while already awake |
| every wake is **paired** with a baseline through the identical client | the first real run showed ~100 ms of each 336 ms "wake" was `curl`'s own startup |
| overhead is measured against **the same container without the wake path**, interleaved | a separately published nginx folds two different containers into the delta; and measuring all the floor then all the through path lets load drift land in the answer - this floor moved 660 µs → 4280 µs between two runs on one machine |
| a delta inside the harness's own jitter is **not published as a number** | the jitter here is ±150-900 µs and the proxy tax is ~15 µs, so this harness cannot resolve it and says so |

`N/A` and `SKIPPED` are different facts. Sablier has no postgres row because it is HTTP-only
by design - that is a *result*.

zeropod took two attempts to gate honestly.

It does not stop the container - the pod stays `Running` while checkpointed - so `kubectl get
pod` cannot tell asleep from awake, and a wake timed without that distinction is a warm request
wearing a wake's name.

`scripts/zeropod-probe.sh` gates on the `zeropod_running` metric instead (0 when checkpointed),
scraped from inside the cluster. That is what turned "cannot be measured" into the 272 ms row
below.

### Measured · 2026-08-15

Conditions printed by the run, copied from the artifact rather than remembered:
darwin/arm64, **host load 5.37**, 285 MB free in the VM, docker 29.2.1, **noise floor
380 µs/req ±90 µs**. Idle windows differ per arm and print too - sbx 5 s, Sablier 60 s,
Lazytainer 10 s. This is still a loaded laptop; the numbers below are a comparison taken
under one set of conditions, not a specification.

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

sbx served the first attempt every time, on both targets. That is the whole claim this
project makes, and until this run it had never been measured against anything.

Its 3286 ms is also not a latency to compare with ours: it is gated by its own 3 s poll
rate, which is why its spread is 43 ms against our 19 ms. Different mechanisms, not a
faster or slower version of the same one.

**Overhead: 33 µs/req over a same-container floor**, jitter ±21 µs - the first run in which
this harness could resolve the proxy tax at all. It is the same quantity `proxy_bench_test.go`
puts at ~15 µs by a different method: benchstat times a bare loopback echo, this times HTTP
through a real container, so the two are close rather than equal and neither replaces the
other. Rows without a same-container baseline print `n/a` instead of a number - an earlier
version compared a rival against a *separately published* nginx and produced −852 µs/req,
faster than direct, which is an artifact of comparing two containers.

**Still unmeasured, and stated rather than omitted:** Sablier's wake path, because its
Traefik middleware would not engage under any plugin configuration tried; and Lazytainer's
nginx arm on this run. That is all of it - zeropod used to be on this list and is now the
272 ms row above, which is the only time this project has independently confirmed a rival's
published claim rather than disputing one.

---

## Conditions matter

`scripts/bench.sh` prints host load and VM memory alongside its results, because a wake on
an idle laptop and a wake on one that is paging are not the same measurement. Several
figures in this project's history were taken during a period when the VM had 107 MB free and
a load average of 20; they were wrong by an order of magnitude and are not in this file.

**A worked example, from the session that wrote most of this page.** After several hours of
end-to-end suites the same machine sat at load average 9 with 30 containers and 484 volumes,
and `bench.sh 20` returned a median of 262 ms with a standard deviation of 314 ms - against
the 191 ms / stdev 24 ms above. The number above was not replaced, for two reasons worth
stating:

- **A noisy measurement does not refute a clean one.** The stdev is thirteen times larger;
  what it measures is the machine, not the wake.
- **Whether the code regressed is a different question, and it has a different answer.** An
  interleaved A/B of the two builds on that same loaded machine - order alternating, n=14 -
  showed no difference outside the noise in either direction. A paired comparison survives
  conditions that destroy an absolute one, which is why the harness is built that way.

So the honest position is: 191 ms stands as measured under the conditions named beside it, and
nothing since has been shown to move it.
