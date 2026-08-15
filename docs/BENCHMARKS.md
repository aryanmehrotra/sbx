# Benchmarks

Every number here was measured on the machine described beside it, by a script in this repo
that you can run. Nothing is quoted from a single run.

```sh
scripts/bench.sh 20                                  # wake latency, distribution
go test -run '^$' -bench RoundTrip -count 12 .       # proxy overhead, for benchstat
./sbx selftest                                       # the whole cycle, ~9s
./scripts/e2e.sh 3                                   # several sandboxes at once
./scripts/recovery.sh                                # kill the daemon, twice
```

---

## Wake

| | median | detail |
|---|---|---|
| docker | **191 ms** | n=20, p90 232 ms, stdev 24 ms |
| kubernetes | **1534 ms** | n=5, min 1362, max 2060 — a pod must be scheduled |

These are a laptop and a minikube. For scale against hosted platforms, see
[against other platforms](#against-other-platforms) below — including why that comparison is
weaker than it looks.

### How it got there

The first honest measurement was **5282 ms, stdev 22 ms**, and the consistency was the
finding — 0.4% spread is not variable work, it is one fixed cost:

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

---

## Memory

Both containers fresh, both idle, same image — which is the only comparison that means
anything:

| | stock | tuned |
|---|---|---|
| `mysql:8.0` | 411 MB | **110 MB** |
| `clickhouse:24.3` | 199 MB | 201 MB |
| a sleeping sandbox | — | **0 B** |
| the daemon, at rest | — | **9.1 MB** |
| the daemon, fronting one sandbox | — | 9.6 MB |
| the daemon, after traffic | — | 10.4 MB |

**Corrected 2026-08-15: the daemon was published at 4.5 MB and that figure is wrong.**
Measured by `ps -o rss` on this build: 9296 KiB with no sandboxes at all, 9808 KiB fronting
one, 10640 KiB after a wake and some traffic. It is not a matter of different quantities —
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
| Neon | 300–800 ms | Postgres data | [vendor docs][neon] |
| Fly, suspended | a few hundred ms | RAM snapshot | [vendor docs][fly] |
| Fly, stopped | ~2000 ms+ | disk | [vendor docs][fly] |
| Daytona | ~90 ms p99 *reported* | disk, persistent volume | third-party 2026 roundup |
| Knative | pod schedule, seconds | volume if attached | — |

[e2b]: https://docs.e2b.dev/sandbox/persistence
[neon]: https://neon.com/docs/connect/connection-latency
[fly]: https://fly.io/docs/reference/suspend-resume/

### Why these numbers don't belong in the same table

They are here because people ask, and refusing to answer is its own kind of dishonesty. But
four things make the column non-comparable, and all four favour us:

1. **Different hardware.** Ours is one laptop with nothing else running. Theirs is a
   multi-tenant fleet serving other people at the same time.
2. **No network.** 191 ms is loopback. Every hosted figure is a client somewhere else on the
   internet, and the [Neon docs are explicit][neon] that physical distance dominates.
3. **Different work.** E2B's ~1 s restores a memory image with your processes inside it. Our
   191 ms starts a process against a warm disk. **Theirs is the harder problem**, and a
   like-for-like row would compare their number to a cold `docker start` plus a health check
   — which is exactly what our 5282 ms first measurement was.
4. **They publish a floor, we publish a distribution.** n=20, p90 and stdev are in the row
   above. A vendor's "~1 s" has no n.

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
| a sample counts only on a **correct protocol reply** | Sablier's middleware failed to engage during development and returned **502 in 98 ms** — faster than sbx's real wake. A status code is not evidence |
| a sample is **VOID** unless the target was verifiably asleep at `t0` | otherwise a rival whose mechanism never engaged scores a spectacular wake for answering while already awake |
| every wake is **paired** with a baseline through the identical client | the first real run showed ~100 ms of each 336 ms "wake" was `curl`'s own startup |
| overhead is measured against **the same container without the wake path**, interleaved | a separately published nginx folds two different containers into the delta; and measuring all the floor then all the through path lets load drift land in the answer — this floor moved 660 µs → 4280 µs between two runs on one machine |
| a delta inside the harness's own jitter is **not published as a number** | the jitter here is ±150–900 µs and the proxy tax is ~15 µs, so this harness cannot resolve it and says so |

`N/A` and `SKIPPED` are different facts. Sablier has no postgres row because it is HTTP-only
by design — that is a *result*. zeropod has none because nothing yet distinguishes
checkpointed from running — that is not.

### Measured · 2026-08-15

Conditions printed by the run, copied from the artifact rather than remembered:
darwin/arm64, **host load 5.37**, 285 MB free in the VM, docker 29.2.1, **noise floor
380 µs/req ±90 µs**. Idle windows differ per arm and print too — sbx 5 s, Sablier 60 s,
Lazytainer 10 s. This is still a loaded laptop; the numbers below are a comparison taken
under one set of conditions, not a specification.

| contender | target | n | median | paired delta | **first attempt served** | overhead | resident |
|---|---|---|---|---|---|---|---|
| **sbx** | nginx | 5 | **174 ms** | **116 ms** | **5/5** | 33 µs/req ±21 µs | 13.9 MB `ps` |
| **sbx** | postgres | 5 | 931 ms | 511 ms | **5/5** | n/a | 13.0 MB `ps` |
| Lazytainer | postgres | 5 | 3286 ms | 3198 ms | **0/5** | n/a | 10.2 MB `docker stats` |
| Lazytainer | nginx | — | SKIPPED | — | — | — | could not be stood up on this run |
| Sablier | nginx | — | SKIPPED | — | — | — | middleware did not block: a request to a stopped target failed instead of waiting |
| Sablier | postgres | — | **N/A** | — | — | — | HTTP-only by design — a middleware on an HTTP request cannot wake a `psql` client |
| zeropod | both | — | omitted | — | — | — | no observable distinguishes checkpointed from running |

**The column that matters is "first attempt served", not the milliseconds.**

Lazytainer wakes on a *packet threshold*. Measured directly: attempts 1–5 were refused in
about a millisecond each, and the sixth was served 5150 ms after the first. It never holds
the connection. So a client that does not retry — `psql`, a connection pool, a test runner
somebody else wrote — does not get a slow response from it. It gets a failure.

sbx served the first attempt every time, on both targets. That is the whole claim this
project makes, and until this run it had never been measured against anything.

Its 3286 ms is also not a latency to compare with ours: it is gated by its own 3 s poll
rate, which is why its spread is 43 ms against our 19 ms. Different mechanisms, not a
faster or slower version of the same one.

**Overhead: 33 µs/req over a same-container floor**, jitter ±21 µs — the first run in which
this harness could resolve the proxy tax at all. It is the same quantity `proxy_bench_test.go`
puts at ~15 µs by a different method: benchstat times a bare loopback echo, this times HTTP
through a real container, so the two are close rather than equal and neither replaces the
other. Rows without a same-container baseline print `n/a` instead of a number — an earlier
version compared a rival against a *separately published* nginx and produced −852 µs/req,
faster than direct, which is an artifact of comparing two containers.

**Still unmeasured, and stated rather than omitted:** Sablier's wake path, because its
Traefik middleware would not engage under any plugin configuration tried; Lazytainer's nginx
arm on this run; and zeropod entirely.

---

## Conditions matter

`scripts/bench.sh` prints host load and VM memory alongside its results, because a wake on
an idle laptop and a wake on one that is paging are not the same measurement. Several
figures in this project's history were taken during a period when the VM had 107 MB free and
a load average of 20; they were wrong by an order of magnitude and are not in this file.
