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
| the daemon | — | 4.5 MB ⚠️ **contradicted below** |

⚠️ **The 4.5 MB daemon figure does not reproduce and is under correction.** On 2026-08-15
`scripts/compare.sh` measured the same daemon at **9.2 MB** (nginx run) and **11.8 MB**
(postgres run) via `ps -o rss` while it was proxying a live sandbox — roughly two to three
times the published number. The two are not necessarily the same quantity: 4.5 MB was a
daemon at rest with nothing attached, and these are a daemon with a listener, a splice and a
wake policy running under memory pressure. That is exactly the kind of "different quantity,
same column" mistake this file retracted the ClickHouse figure for, so **neither number
should be quoted until both are re-measured with the state stated beside them.** Do not cite
4.5 MB in the meantime; it is in the README.

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

`N/A` and `SKIPPED` are different facts. Sablier has no postgres row because it is HTTP-only
by design — that is a *result*. zeropod has none because nothing yet distinguishes
checkpointed from running — that is not.

### First run · 2026-08-15 — *not the comparison table*

⚠️ **This is a harness smoke run, not the cross-contender comparison.** The plan gates that
comparison on a zeropod probe (`ubuntu-latest`/amd64, where its arm64-on-macOS caveat does not
apply) which **has not been run**. Publishing a field comparison that measures the two weaker
rivals and omits the one that beats us would be the flattering outcome even with every printed
number honest — so no such table is published here yet.

⚠️ **Not headline numbers either.** The host was at load 6.95 with 268 MB free in the VM — the
condition the next section says produced figures "wrong by an order of magnitude". Below is
what the harness did and what it refused to do, nothing more.

| contender | target | status | n | median | paired delta | what comes back |
|---|---|---|---|---|---|---|
| **sbx** | nginx | OK | 5 | 398 ms | **191 ms** | disk-warm, process cold |
| **sbx** | postgres | OK | 5 | 683 ms | **504 ms** | disk-warm, process cold |
| Sablier | nginx | SKIPPED | — | — | — | middleware did not block: a request to a stopped target failed rather than waiting |
| Sablier | postgres | **N/A** | — | — | — | HTTP-only by design — a middleware on an HTTP request cannot wake a `psql` client |
| Lazytainer | both | SKIPPED | — | — | — | never slept in 75 s; no group discovered from `LAZYTAINER_GROUP_*`, config format unverified |

**zeropod appears in no row at all, deliberately.** It CRIU-checkpoints while the pod stays
phase `Running`, so neither `docker inspect` nor `kubectl get pod` can express "asleep". With
no gate, every sample would either void (flattering by omission) or record a *warm* request as
a wake (worse). A `SKIPPED` row would read as a bad day; the true fact is that the strongest
rival's mechanism cannot currently be gated at all, and a dash in a table does not say that.

Four of six printed rows are a refusal. **Postgres is the row that matters** — raw TCP, the
case that separates this from every HTTP-middleware tool, and Sablier's `N/A` there is the
measured version of a claim this project had only ever made in prose.

The p90 and stdev on the two OK rows (1785 ms, 636 ms) are the machine, not the software.
Re-run on an idle host before quoting anything from here.

---

## Conditions matter

`scripts/bench.sh` prints host load and VM memory alongside its results, because a wake on
an idle laptop and a wake on one that is paging are not the same measurement. Several
figures in this project's history were taken during a period when the VM had 107 MB free and
a load average of 20; they were wrong by an order of magnitude and are not in this file.
