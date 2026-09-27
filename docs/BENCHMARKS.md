# Benchmarks

Every number sbx publishes, how it was measured, and the command to measure it yourself. Unless
a row says otherwise: sbx v0.14.0 on one Linux machine, 2026-09-27 (details in the
[footnotes](#how-these-were-measured)).

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="bench-dark.svg">
  <img src="bench-light.svg" width="900" alt="Memory held by 20 idle Postgres databases: sbx asleep 17.6 MB, docker compose always on 629 MB. Time until a sleeping Postgres answers psql: sbx 348 ms, first try served 20 of 20; Lazytainer 3,407 ms, first try refused 0 of 5.">
</picture>

## Head to head

Same machine, same images, same clients. Best result in each row in bold.

| | **sbx** | docker compose | Lazytainer | Sablier |
|---|---|---|---|---|
| **RAM held by 20 idle Postgres databases** ¹ | **17.6 MB** | 629 MB | not measured | not measured |
| **Sleeping Postgres answers `psql`** (median) ² | **348 ms** | never sleeps | 3,407 ms | cannot (HTTP only) |
| **First connection served while waking** ² | **20 of 20** | never sleeps | 0 of 5 | cannot (HTTP only) |
| **Sleeping nginx answers `curl`** (median) ² | **240 ms** | never sleeps | 3,061 ms | could not be set up |
| **Extra time per query once awake** ³ | +9.6 µs | **none** | not measured | not measured |
| **What wakes it** | any TCP connection | nothing, always on | a burst of packets | an HTTP request |

What this means for you:

- Idle branches cost memory only with docker compose. Twenty idle Postgres databases hold 629 MB
  there and 17.6 MB with sbx, which is the sbx daemon alone.
- A sleeping sandbox answers the first query. Lazytainer refused the first attempt every time, so a
  client that doesn't retry gets an error instead of a result.
- Once awake, sbx adds microseconds per query. docker compose adds nothing, because nothing sits in
  between; that is the price of sleeping.

## sbx by itself

| Measure | Result | Runs |
|---|---|---|
| Wake, Redis | **216 ms** median (p90 236) | 20 |
| Wake, Postgres | **348 ms** median (p90 488) | 20 |
| Wake, nginx | **240 ms** median (p90 261) | 20 |
| Wake, headless Chrome, cold / warm page cache | **611 / 387 ms** median | 5 each |
| Resume from `"on_idle": "freeze"` (paused, not stopped) | **34 ms** median, 12 ms more than when awake | 20 |
| `sbx create`, image already pulled | **362 ms** median (p90 375) | 10 |
| RAM of a sleeping sandbox | **0 B**, no container running | 3 |
| RAM of the daemon, idle | **12.8 MB** | 3 |
| New connection to an awake sandbox | **+0.17 ms** over a direct connection | 3 × 20 |
| Throughput through the proxy | **1.39 GB/s**, 55% of direct | 10 |
| OpenSandbox create → first command, warm pool | **12.8 ms** median (p95 77.8) | 10 |
| The same, no pool | **342 ms** median | 10 |
| The same, 100 at once from the pool | **309-460 ms** median | 2 × 100 |
| The same on Firecracker microVMs, frozen pool ⁴ | **144 ms** median | 3 × 4 |
| The same on microVMs, asleep pool / no pool ⁴ | **759 / 2,637 ms** median | 3 × 4 |

A wake is mostly the service's own startup, which is why Postgres takes longer than Redis and Chrome
longer than both.

## How these were measured

- **Machine.** A cloud VM with 4 vCPU (Intel Xeon, 2.1 GHz) and 15 GiB RAM, Linux 6.18, Docker
  29.3.1, Go 1.26.0, sbx built from v0.14.0 (`8dff521`). A VM is noisier than bare metal. No KVM
  and no Kubernetes, so those rows come from elsewhere (⁴ and [earlier runs](#earlier-runs)).
- **Rules.** Medians, not means. Images are pulled first, so no timing includes a download. Scripts
  run one at a time. A sample counts only when the client gets a correct protocol reply from a
  target that was verified asleep. A difference smaller than the run-to-run spread is not reported.
- **Timer.** Wake and create times include the harness's own timer, about 12 ms.
- ¹ docker compose side: 20 containers of the postgres template's image and settings, summed from
  `docker stats` 30 s after all accept connections. sbx side: 20 sandboxes from
  `--template postgres`, all asleep with no container running, so the total is the daemon's RSS.
  Two runs: 667 and 629 MB against 17.4 and 17.6 MB. The table uses the smaller docker figure.
- ² `scripts/compare.sh`, each tool against the same target on the same machine. sbx ran 20 times,
  the others 5. Lazytainer wakes on a packet threshold and drops the first connections. Sablier is
  an HTTP middleware, so it cannot wake a `psql` client, and its nginx setup did not work here.
  zeropod needs Kubernetes; CI measured it at 272 ms, 4 of 4 served.
- ³ An in-process benchmark on loopback (19.1 µs proxied against 9.4 µs direct). Against a real
  query that takes 426 µs, it was +7% on an Apple M4.
- ⁴ GitHub's `ubuntu-24.04` runner with nested KVM and Firecracker's jailer on, in CI's `microvm`
  job. No bare-metal run exists yet.
- Every release also attaches a `bench.md` with the proxy benchmarks from its CI runner, for
  comparing one release with the last.

## Run them yourself

```sh
go build -o sbx .
scripts/bench-fleet-memory.sh 20                  # ¹ 20 idle databases, both ways
CONTENDERS=sbx scripts/compare.sh 20              # ² sbx against the same targets
CONTENDERS=lazytainer,sablier scripts/compare.sh 5
scripts/bench.sh 20                               # wake, Redis
scripts/bench-freeze.sh 20                        # resume from freeze
scripts/bench-chrome.sh                           # headless Chrome
scripts/bench-create.sh 10                        # sbx create
scripts/bench-memory.sh                           # daemon and sleeping-sandbox RAM
scripts/connbench.sh 20                           # new-connection overhead
go test -run '^$' -bench 'RoundTrip|Conn|Stream' -count 10 ./internal/daemon   # ³ proxy
scripts/osb-bench.sh --burst 1 --rounds 10 --pool node:22-slim=8 --burst-modes default,cold
scripts/osb-bench.sh --burst 100 --rounds 1 --pool node:22-slim=100
```

## Earlier runs

Kept with the machine they ran on. A number from a different machine is not a speed-up or a
regression.

| What | Result | Version · machine |
|---|---|---|
| Wake, Redis | 191 ms (p90 232) | v0.1.0 · laptop, 2026-08-15 |
| Wake, Postgres / nginx | 931 / 174 ms | v0.1.0 · Apple Silicon Mac under load |
| First connection served, sbx vs Lazytainer | 5 of 5 vs 0 of 5 | v0.1.0 · Apple Silicon Mac |
| Wake, zeropod, nginx | 272 ms, 4 of 4 served | CI, Kubernetes |
| Wake, Kubernetes | 1534 ms (1362-2060) | v0.1.0 · minikube |
| Wake, headless Chrome, cold / warm | 3744 / 766 ms | v0.1.0 · Apple Silicon Mac |
| Wake, microVM on a Mac through the helper VM | 216 ms to first byte | v0.11.0 · Apple M4, colima |
| `sbx create`, microVM on a Mac | 11.5 s | v0.11.0 · Apple M4 (before v0.14 removed the image copy) |
| RAM of the daemon, idle | 9.1 MB | v0.1.0 · macOS laptop |
| Idle MySQL 8, stock / tuned | 411 / 110 MB | v0.1.0 · macOS laptop |
| Idle ClickHouse 24.3 | 199 MB | v0.1.0 · macOS laptop |
| Extra time per query once awake | +14 µs | v0.8.0 · Apple M4 |
| Throughput through the proxy | 7.0 GB/s, 56% of direct | v0.8.0 · Apple M4 |
| OpenSandbox warm pool, one / 100 at once | 11.1 / 412.6 ms (13.7 / 472.1 ms re-keying every claim) | v0.10.0 · Apple M4 |
| OpenSandbox on microVMs, frozen / asleep / no pool | 141 / 699 / 2,821 ms | v0.13.0 · CI `microvm` job |

## Not measured yet

- A microVM wake on bare-metal Linux. Published Firecracker restore figures suggest 4-28 ms; only a
  bare-metal run can confirm it.
- Windows (WSL2) hosts, Kata on Kubernetes, and many microVMs restoring at once.
