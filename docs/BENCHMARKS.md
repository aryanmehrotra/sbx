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
| the daemon | — | 4.5 MB |

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

## Conditions matter

`scripts/bench.sh` prints host load and VM memory alongside its results, because a wake on
an idle laptop and a wake on one that is paging are not the same measurement. Several
figures in this project's history were taken during a period when the VM had 107 MB free and
a load average of 20; they were wrong by an order of magnitude and are not in this file.
