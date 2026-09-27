# Benchmarks

Every number sbx publishes, how it was measured, and the command to measure it yourself. Unless
a row says otherwise: sbx v0.14.0 on one Linux machine, 2026-09-27 (details in the
[footnotes](#how-these-were-measured)).

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="bench-dark.svg">
  <img src="bench-light.svg" width="900" alt="Memory held by 20 idle Postgres databases: sbx asleep 17.6 MB, docker compose always on 629 MB. Time until a sleeping Postgres answers psql: sbx 348 ms, first try served 20 of 20; Lazytainer 3,407 ms, first try refused 0 of 5. OpenSandbox API, create a sandbox and run a first command: sbx 307 ms, OpenSandbox server 1,417 ms.">
</picture>

## Head to head

Two kinds of tool compete with sbx, so there are two tables. Each was measured on the same
machine, with the same images and clients. Best result in each row in bold.

### Databases for branches and tests

| | **sbx** | docker compose | Testcontainers | Lazytainer | Sablier |
|---|---|---|---|---|---|
| RAM held by 20 idle Postgres databases ¹ | **17.6 MB** | 629 MB | not measured | not measured | not measured |
| Sleeping Postgres answers `psql` (median) ² | **348 ms** | never sleeps | never sleeps | 3,407 ms | cannot (HTTP only) |
| First connection served while waking ² | **20 of 20** | never sleeps | never sleeps | 0 of 5 | cannot (HTTP only) |
| Sleeping nginx answers `curl` (median) ² | **240 ms** | never sleeps | never sleeps | 3,061 ms | could not be set up |
| Brand-new Postgres answers its first query (median) ⁵ | 4,975 ms | **2,498 ms** | **2,520 ms** | not measured | not measured |
| Extra time per query once awake ³ | +9.6 µs | **none** | **none** | not measured | not measured |
| What wakes it | any TCP connection | nothing, always on | nothing, always on | a burst of packets | an HTTP request |

- Idle databases cost memory only when they never sleep: 629 MB for twenty with docker compose,
  17.6 MB with sbx, which is the sbx daemon alone.
- A sleeping sbx sandbox answers the first query. Lazytainer refused the first attempt every time.
- A brand-new database per test is not faster with sbx today. `sbx with` waits for the daemon's
  15 s refresh, so it took 5.0 s against 2.5 s. Reusing a sleeping sandbox took 289 ms.
- Once awake, sbx adds microseconds per query. The always-on tools add nothing.

### Sandboxes for AI agents

| | **sbx** | OpenSandbox server |
|---|---|---|
| Create → first command (median) ⁶ | **307 ms** | 1,417 ms |
| Command round trip (median) ⁶ | **3.3 ms** | 1,005.5 ms |
| Pause → resume → first command (median) ⁶ | **29.5 ms** | 1,048 ms |
| Server RAM with 10 idle sandboxes ⁶ | **16 MiB** | 165 MiB |
| Create → first command from a warm pool | **12.8 ms** | no pool on Docker |

Both run the same API, so the same SDK and image drive both. Every command on the upstream server
took about a second to return (footnote ⁶), which accounts for most of its create figure.

### Hosted sandboxes, published figures

Not measured here. [ComputeSDK](https://github.com/computesdk/benchmarks) times create → first
command (`node -v`) for 100 sandboxes created at once, from GitHub Actions runners over the
internet. Its run of 2026-09-25, next to sbx's figure for the same shape on one machine with no
network in between:

| | Median, 100 at once | Idle cost |
|---|---|---|
| sbx, warm pool (measured here) | 309-460 ms (2 runs) | $0, 0 B RAM |
| sbx, no pool (measured here) | 8,971 ms (1 run) | $0, 0 B RAM |
| isorun | 43.6 ms | not checked |
| Daytona | 341.4 ms | storage |
| Vercel Sandbox | 452.9 ms | snapshot storage |
| Cloudflare Sandbox | 647.8 ms | $0; files are deleted on sleep |
| Modal | 907.6 ms | snapshot storage |
| E2B | 1,237.7 ms | $0 while paused |
| microsandbox | 2,541.3 ms | $0, your hardware |

None of these wakes on a plain client connection: they resume through their SDK, API or an HTTP
request. Sources, dates and idle-cost links are in [COMPARISON.md](COMPARISON.md).

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
| The same, 100 at once with no pool | **8,971 ms** median (p95 17,711) | 1 × 100 |
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
- ⁵ `scripts/bench-pg-per-test.sh 10`, sbx built from `5f78032` (v0.14.0 plus 18 commits),
  Testcontainers 4.15.0 (Python), Docker Compose 5.1.1. Ten rounds, contenders interleaved and
  rotated. Same digest-pinned image as `examples/postgres`, same credentials, and the host's `psql`
  running `select 1` as the only client. Testcontainers is timed in-process from `start()`, with
  its reaper off, since a test session pays both once. Compose uses the template's health check
  at its 300 ms pace, `start_interval` included. Spreads: compose 2,442-2,526, Testcontainers
  2,498-2,557, so the two are not resolvable. `sbx with` ran 2,311-5,232: four runs landed right
  after the daemon's refresh tick and took under 2.8 s. The same 10 runs against
  `sbx serve --refresh 1s`, not interleaved, took 2,352 ms median (2,117-2,538). A wake of an
  existing sleeping sandbox in the same rounds took 289 ms median (259-442); it is lower than the
  348 ms above because this `psql` runs on the host, not in a client container.
- ⁶ `scripts/bench-osb-upstream.sh 10 10`: upstream's `opensandbox/server:release-1.1.0` image
  (the release in `test/osb/UPSTREAM`) with `opensandbox/execd:v1.1.0`, run with the docker
  socket as upstream's `server/docker-compose.example.yaml` does, against sbx built from
  `5f78032`. Upstream's Go SDK v1.1.0 as the client, image `node:22-slim`, 10 rounds interleaved
  and rotated. Create → first command spread: sbx 260-913, upstream 1,398-2,424. Command round
  trip, 100 each: sbx 2.5-7.6, upstream 1,004-1,019. Upstream's execd sends
  `execution_complete` 2 ms into a `true` and closes the stream at 1.0 s, seen with `curl`
  directly, so each command costs a second; the cause was not traced further. Other lifecycle
  medians, sbx against upstream: upload 1 MiB 4.1 / 11.2 ms, download 9.3 / 8.9 ms (not
  resolvable), pause → resume → first command 29.5 / 1,048 ms. ComputeSDK's TTI shape (one
  create → `node -v` per round, 10 rounds each, alternating, a fresh sbx daemon per round): sbx
  620 ms median (566-5,737, the first round of the run the outlier), upstream 1,488 ms
  (1,447-1,612). Upstream's docker runtime has no warm pool, so there is no pooled pair. Memory:
  10 sandboxes left untouched 60 s, still running on both (sbx's `--idle` is 5 min): 34 MiB of
  containers with sbx, 40 MiB with upstream, from `docker stats`. Server RSS: sbx daemon 16 MiB,
  upstream's Python server 165 MiB.
- Not run here: Daytona's open-source repository "is no longer maintained"
  ([its README](https://github.com/daytonaio/daytona), checked 2026-09-27), and its last
  self-hosting compose file (v0.190.0) is 14 services with a privileged runner. E2B's self-hosted stack and microsandbox both need KVM, which
  this machine does not have.
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
scripts/bench-pg-per-test.sh 10                   # ⁵ Testcontainers, compose, sbx with, sbx wake
scripts/bench-osb-upstream.sh 10 10               # ⁶ OpenSandbox's own server against sbx
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
