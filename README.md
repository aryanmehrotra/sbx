# sbx

<img src="docs/hero.svg" width="900" alt="sbx: a real database for every branch and AI agent. It sleeps at 0 B of RAM and wakes in 216 ms for Redis or 348 ms for Postgres (median of 20, v0.14.0, Linux x86_64, docker). A diagram shows psql connecting, sbx holding the connection open while Postgres wakes from 0 B, then the same socket served, never refused. No SDK, self-hosted, one static binary, zero dependencies.">

**Self-hosted sandboxes for every branch and AI agent — a real Postgres, Redis or browser that
sleeps at 0 B of RAM and wakes the moment anything connects. No SDK required, no account, one binary.**

<sub>Not Docker Sandboxes' `sbx` CLI (`docker/tap/sbx`), which runs coding agents in microVMs. This is `aryanmehrotra/sbx`.</sub>

[![CI](https://github.com/aryanmehrotra/sbx/actions/workflows/ci.yaml/badge.svg)](https://github.com/aryanmehrotra/sbx/actions/workflows/ci.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/aryanmehrotra/sbx.svg)](https://pkg.go.dev/github.com/aryanmehrotra/sbx)
[![Go Report Card](https://goreportcard.com/badge/github.com/aryanmehrotra/sbx)](https://goreportcard.com/report/github.com/aryanmehrotra/sbx)
[![release](https://img.shields.io/github/v/release/aryanmehrotra/sbx?color=3fb950)](https://github.com/aryanmehrotra/sbx/releases)
[![dependencies](https://img.shields.io/badge/dependencies-0-3fb950)](go.mod)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

## Install

```sh
brew install aryanmehrotra/tap/sbx
# or: curl -fsSL https://raw.githubusercontent.com/aryanmehrotra/sbx/main/scripts/install.sh | sh
# or: go install github.com/aryanmehrotra/sbx@latest
```

One static binary for macOS and Linux (amd64, arm64); on Windows, run it inside WSL2. It drives
the Docker engine you already have. `sbx doctor` says what this machine can do.

**Status:** pre-1.0. What to do before each upgrade is in the [release notes](docs/release-notes/README.md);
what has been run where is in [Platform status](#platform-status).

## Quickstart

### A database for every branch or agent

```sh
sbx serve --idle 5m &                          # the daemon: once per machine, it owns the ports
sbx create my-branch --template postgres       # a real Postgres 16 for this branch
eval "$(sbx env my-branch)"                    # sets PGHOST, PGPORT, DATABASE_HOST, DATABASE_PORT
PGPASSWORD=app psql -U app -d app              # connecting wakes it; the call waits, never refuses
```

After five idle minutes it sleeps at 0 B of RAM. The next `psql` wakes it again. There is no
`sbx start` and no `sbx stop`. The [10-minute quickstart](docs/QUICKSTART.md) adds snapshots,
forks and an AI agent.

### Run your agent's code in a sandbox

[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) is an open-source API standard
for AI-agent sandboxes, with SDKs in 5 languages. sbx serves that API, so its SDKs run against
your own machine unchanged:

```sh
sbx serve --osb-addr 127.0.0.1:8080 --osb-pool python:3.11-slim=4 &   # the API, plus 4 warm sandboxes
pip install opensandbox
export OPEN_SANDBOX_DOMAIN=127.0.0.1:8080 OPEN_SANDBOX_API_KEY="$(cat ~/.sbx/osb/key)"
```

```python
from opensandbox import SandboxSync

sandbox = SandboxSync.create("python:3.11-slim")    # answered from the warm pool
try:
    run = sandbox.commands.run("python -c 'print(6 * 7)'")
    print(run.logs.stdout[0].text)                   # 42
finally:
    sandbox.kill()
```

One daemon per machine does both jobs: if the one above is running, restart it with these flags.
Each sandbox is a container by default; `--provider firecracker` gives each one its own kernel.
MCP tools and the other SDKs are in [GUIDES.md](docs/GUIDES.md#ai-agents).

## Why teams choose sbx

- **Your tools work unmodified.** Most sandboxes wake when *your code* calls their SDK. sbx wakes
  when *any client* connects: it holds the first TCP connection open while the service starts,
  then hands over the live socket. `psql`, a connection pool or Playwright just work. Proof: 20 of
  20 first connections served on a sleeping Postgres; Lazytainer, a similar tool, served 0 of 5.
- **Idle costs nothing.** A sleeping sandbox runs no container: 0 B of RAM, measured. Twenty
  branch databases on one laptop cost only the one you are using. The daemon itself is 12.8 MB.
- **Existing OpenSandbox code runs as is.** OpenSandbox's own end-to-end test suite runs
  unmodified against sbx in CI, at a pinned upstream commit ([test/osb](test/osb/README.md)).
  From a warm pool, create to first command takes 12.8 ms.
- **Nothing to trust but your machine.** Self-hosted, MIT licensed, no account, nothing hosted.
  One static Go binary with zero third-party dependencies (`go.mod` has no `require`).
- **One spec from laptop to cluster to microVM.** The same `sandbox.json` runs on Docker,
  Kubernetes, or a jailed Firecracker microVM, and declares several services at once.

<img src="docs/how-it-works.svg" width="900" alt="How sbx wakes a sandbox, in three steps. 1: psql connects to a port that belongs to sbx while the Postgres behind it is asleep, using 0 B of memory. 2: sbx accepts the connection and holds it open while the service starts. 3: once Postgres is healthy, sbx hands over the live connection and the query is answered. Any TCP protocol, unmodified clients.">

<img src="docs/demo.svg" width="900" alt="A terminal recording of sbx: a sandbox is created from the web-stack template, sleeps to 0 B when nobody connects, and a plain psql query wakes it and is served in 435 ms without an error. Then an agent reads its addresses as JSON, a redis cache is added mid-task, and a seeded database is snapshotted and forked.">

<sub>A real run, recorded by [`scripts/demo.sh`](scripts/demo.sh).</sub>

## Which one should I use?

| You want | Use |
|---|---|
| A database or stack per branch, PR or agent, on your own machine | **sbx** |
| Unmodified clients (`psql`, pools, Playwright) to wake a sleeping service | **sbx** |
| OpenSandbox or E2B-style agent sandboxes, self-hosted, from one binary | **sbx** |
| Throwaway containers controlled from inside test code, per language | Testcontainers, or `sbx with` for any language |
| A few always-on services for one project, nothing to learn | docker compose |
| Nothing to run yourself | E2B, Daytona, Modal or Vercel (hosted) |
| Only Postgres branches, hosted | Neon |

The long version, with sources and where sbx loses: [COMPARISON.md](docs/COMPARISON.md#which-one-should-i-use).

## How it compares

● yes · ◐ partial or conditional · ○ no. Vendor cells checked 2026-09-27 against each vendor's
own pages; sources are in [COMPARISON.md](docs/COMPARISON.md).

| | **sbx** | E2B | Daytona | OpenSandbox | Modal | Fly |
|---|:---:|:---:|:---:|:---:|:---:|:---:|
| Wakes on an unmodified client connection | ● any TCP | ○ SDK | ○ API | ○ API | ○ SDK | ◐ HTTP¹ |
| Self-hosted, no account | ● MIT | ◐ infra | ◐ frozen² | ● | ○ | ○ |
| Hosted, nothing to run | ○ | ● | ● | ○ | ● | ● |
| Sleep keeps RAM + processes | ● microVM · ◐ docker³ | ● | ◐ VM only | ● | ◐ alpha | ● suspend |
| Isolation | container · jailed microVM | Firecracker | container; VM optional | container to Firecracker | gVisor | Firecracker |
| SDKs | OpenSandbox's 5 · CLI · MCP | Python, JS | 5 languages | 5 languages | Python, JS, Go | REST API |

¹ Through Fly Proxy; raw TCP needs a dedicated IPv4. ² AGPL; the open-source core stopped
updating in 2026-06. ³ Opt-in `on_idle: "freeze"`, which keeps RAM in use while idle.

sbx loses on hosting, native SDKs and community size. It wins when the client can't call an SDK
and the machine is yours.

## What you can do

| You want | Do |
|---|---|
| Start from a template | `--template postgres`, `browser`, `nginx`, `web-stack`, `analytics` |
| Keep one spec per repo | `sandbox.json`: services, ports, health checks, seed data ([SPEC](docs/SPEC.md)) |
| Hand addresses to any tool | `sbx env` for posix, fish, powershell, cmd or JSON |
| A database for one command | `sbx with` creates, runs, and always removes, even on failure |
| Seed once, fork per agent | `sbx snapshot`, then `sbx fork`; each fork is independent |
| Give agents sandbox tools over MCP | daemon with `--osb-addr 127.0.0.1:8080`, then `claude mcp add sbx -- sbx mcp` |
| Allow only the APIs you name | `egress_allow: ["api.openai.com"]`; change it live with `sbx egress` |
| Cap each service | `cpu`, `memory`, `gpus` per service |
| The same spec on a cluster, or its own kernel | `--provider kubernetes`, `--provider firecracker` ([status](#platform-status)) |
| Watch the fleet | `sbx ui`: state, usage against limits, recent wakes |

Every command and flag: [CLI.md](docs/CLI.md), or `sbx help`.

<img src="docs/ui.svg" width="900" alt="The sbx dashboard: a table of every sandbox and service with its state, cpu and memory against the limit it is allowed, a detail block for the selected service showing its address, connect command and a trace of cpu and memory over time, a log of recent wake and sleep events, and the key hints along the bottom.">

## Performance

Measured by scripts in this repo on v0.14.0, 2026-09-27, on a Linux cloud VM (4 vCPU Xeon, Docker,
not bare metal) unless a row says otherwise. Every figure, its script and older runs are in
[BENCHMARKS.md](docs/BENCHMARKS.md).

| What | Figure |
|---|---|
| First connection to a sleeping Postgres, served | **20/20** (Lazytainer: 0/5) |
| A sleeping sandbox · the daemon at rest | **0 B** of RAM · 12.8 MB |
| Wake, Redis on Docker | **216 ms** median, n=20 |
| Wake, Postgres on Docker | **348 ms** median, n=20 |
| OpenSandbox create → first command, Docker warm pool | **12.8 ms** median, n=10 |
| Same, frozen Firecracker microVM pool | **141 ms** median (v0.13.0, CI runner with nested KVM) |

Wake time is mostly the workload's own startup, which is why Postgres takes longer than Redis.

## Platform status

Every provider takes the same `sandbox.json`. What has actually been run where, at v0.14.0:

| Provider · host | Status |
|---|---|
| docker · Linux | **verified in CI** on every change (`selftest`, `usecases`, `osb-conformance`) |
| docker · macOS (colima, Docker Desktop) | **unit-tested** in CI daily; **run by hand** on an M4 with colima (benchmarks) |
| docker · Windows | inside WSL2 only; **not yet run end to end** on a Windows host |
| docker `--isolation gvisor` | **verified in CI** (`isolation` job) |
| kubernetes | **unit-tested**; **run by hand** on minikube (benchmarks); not in CI |
| kubernetes `--isolation firecracker` (kata-fc) | **unit-tested**; **not yet run end to end** on a cluster |
| firecracker · Linux with `/dev/kvm` | **verified in CI** (`microvm` job, nested KVM, jailer on) |
| firecracker · M3+ Mac, macOS 15+, via helper VM | **run by hand** with colima on an M4 (v0.11); lima **unit-tested** |
| firecracker · Mac, OpenSandbox API through the helper VM | **unit-tested**; **not yet run end to end** on a Mac |
| firecracker · Windows 11, via a WSL2 helper VM | **unit-tested**; **not yet run end to end** on a Windows host |
| `sbx checkpoint` / `resume` | **run by hand** on Linux with podman (v0.7.0); not in CI |

`sbx doctor` reports which of these applies to the machine you're on.

## Docs

| To | Page | What it's for |
|---|---|---|
| **Learn** | [QUICKSTART.md](docs/QUICKSTART.md) | Ten minutes: wake a Postgres with `psql`, fork it, hand sandboxes to an AI agent |
| **Do** | [GUIDES.md](docs/GUIDES.md) | Every task: branches, CI, previews, [AI agents](docs/GUIDES.md#ai-agents) (MCP, SDKs) |
| | [examples/](examples/README.md) | Ready-made `sandbox.json` files, including the built-in templates |
| | [TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md) | A symptom you're seeing, its cause, and the fix |
| **Look up** | [CLI.md](docs/CLI.md) | Every command, `sbx serve` flag and `SBX_*` environment variable |
| | [SPEC.md](docs/SPEC.md) | Every `sandbox.json` field, per provider, and a docker-compose mapping |
| | [BENCHMARKS.md](docs/BENCHMARKS.md) | Every published number, with its machine, date and script |
| **Understand** | [ARCHITECTURE.md](docs/ARCHITECTURE.md) | How it works: the daemon, the wake path, providers |
| | [DECISIONS.md](docs/DECISIONS.md) | Why things are the way they are, one decision per entry |
| | [COMPARISON.md](docs/COMPARISON.md) | sbx against E2B, Daytona, OpenSandbox, Testcontainers and others |
| | [SECURITY.md](SECURITY.md) | What each isolation level protects; how to report a vulnerability |
| **Project** | [ROADMAP.md](docs/ROADMAP.md) | What comes next, and what is ruled out |
| | [Release notes](docs/release-notes/README.md) | What changed in each version, and what to do before upgrading |
| | [CONTRIBUTING.md](CONTRIBUTING.md) · [AGENTS.md](AGENTS.md) | Build and test; the rules for humans and coding agents editing this repo |

## Glossary

<details>
<summary>Terms used on these pages, in plain words</summary>

| Term | Meaning |
|---|---|
| sandbox | One named, isolated copy of a project's services, for one branch, task or agent |
| service | One process inside a sandbox, such as its Postgres or its Redis, with its own port |
| snapshot / fork | Save every service's data once, then make as many independent sandboxes from it as you like |
| preview feature | A feature that is off until you turn it on with `sbx features`, because it may still change |
| E2B, Daytona | Hosted services that rent AI agents a sandbox to run code in; see [COMPARISON](docs/COMPARISON.md) |
| spec | The `sandbox.json` file that declares a sandbox's services ([SPEC](docs/SPEC.md)) |
| template | A built-in spec you pick with `--template`, such as `postgres` |
| daemon | `sbx serve`: the one long-running process that holds the ports and wakes and sleeps sandboxes |
| provider | What runs the sandboxes: `docker` (default), `kubernetes` or `firecracker` |
| sleep | Stop an idle service so it uses no memory or CPU |
| wake | Start a sleeping service because something connected; that first connection waits, it is not refused |
| freeze | Pause a service with its memory kept, so it resumes in about 10 ms instead of restarting |
| OpenSandbox | An open-source API standard for AI-agent sandboxes, with SDKs in 5 languages |
| MCP | Model Context Protocol: the standard way AI assistants such as Claude or Cursor call outside tools |
| microVM | A small virtual machine with its own kernel, so a sandbox does not share the host's |
| Firecracker | The open-source microVM monitor AWS built for Lambda; sbx's `firecracker` provider uses it |
| helper VM | A Linux VM sbx starts on a Mac or Windows machine so Firecracker microVMs can run there |
| gVisor | A runtime that runs containers on its own user-space kernel instead of the host's |
| Kata | Kata Containers: a runtime that runs each container inside its own lightweight VM |
| CRIU | A Linux tool that saves a running process's memory to disk and restores it later |
| warm pool | Sandboxes started ahead of time, so a create is answered at once |
| egress | Traffic leaving a sandbox for the network; sbx can limit it to the hosts you name |

</details>

## Contributing

Issues and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) explains setup and how
the tests are arranged; [AGENTS.md](AGENTS.md) holds the rules for humans and coding agents
editing this repo. Report vulnerabilities as described in [SECURITY.md](SECURITY.md).

MIT licensed. See [LICENSE](LICENSE).
