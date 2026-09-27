# Use cases

Thirteen shapes sbx fits. Each has who it is for, the commands, and where to read more. Every
command and flag is in [CLI.md](CLI.md); every spec field in [SPEC.md](SPEC.md).

All of them assume one daemon per machine:

```sh
sbx serve --idle 5m &
```

| # | case | key commands |
|---|---|---|
| 1 | [Several branches at once](#1--several-branches-at-once) | `create`, `env` |
| 2 | [A service for an agent mid-task](#2--a-service-for-an-agent-mid-task) | `add` |
| 3 | [CI: a stack for a build, a fixture for a test](#3--ci-a-stack-for-a-build-a-fixture-for-a-test) | `ready`, `with` |
| 4 | [Seed once, fork many](#4--seed-once-fork-many) | `snapshot`, `fork` |
| 5 | [Park a process and resume it](#5--park-a-process-and-resume-it) | `checkpoint`, `resume` |
| 6 | [A preview link for somebody else](#6--a-preview-link-for-somebody-else) | `url` |
| 7 | [A browser that sleeps](#7--a-browser-that-sleeps) | `--template browser` |
| 8 | [A sandbox that is not on your laptop](#8--a-sandbox-that-is-not-on-your-laptop) | `pack`, `connect` |
| 9 | [A box to run your own commands in](#9--a-box-to-run-your-own-commands-in) | `exec -t`, `ssh` |
| 10 | [An agent box that only calls out](#10--an-agent-box-that-only-calls-out) | `egress_allow`, `egress` |
| 11 | [The same spec in a cluster](#11--the-same-spec-in-a-cluster) | `--provider kubernetes` |
| 12 | [Untrusted code in a microVM](#12--untrusted-code-in-a-microvm) | `--provider firecracker` |
| 13 | [Existing OpenSandbox SDK code](#13--existing-opensandbox-sdk-code) | `serve --osb-addr` |

---

## 1 · Several branches at once

**Who:** anyone with more than one branch open. Branches share one database, so a migration on one
is a migration on all; a full stack per branch costs memory for every branch you ever opened.

```sh
sbx create feature-x --template postgres
eval "$(sbx env feature-x)"
psql -U app -d app -c 'select 1'   # PGHOST/PGPORT come from sbx env; this connection wakes it
```

A sleeping sandbox holds 0 B of memory. Figures: [BENCHMARKS.md](BENCHMARKS.md).

---

## 2 · A service for an agent mid-task

**Who:** a coding agent that discovers it needs a cache or a second database. Its clients (`psql`,
a pool, a test runner) cannot call an SDK; they open a socket, and the socket is the wake signal.

```sh
sbx create my-task --template postgres
sbx add my-task cache --image redis:7-alpine --port 6379 --health 'redis-cli ping'
```

The new service gets a port from the sandbox's block, sleeps when idle, and is removed with the
sandbox. → [AI-AGENTS.md](AI-AGENTS.md)

---

## 3 · CI: a stack for a build, a fixture for a test

**Who:** a CI job that needs real services. A `create`/`env`/`rm` script leaks the sandbox when a
test panics or the runner is killed.

A fixture that lives exactly as long as the command, removed even on failure or interrupt:

```sh
sbx with test-db --template postgres -- go test ./...
```

A stack a job waits on, kept for the next job on a persistent runner:

```sh
sbx create "$BRANCH" && sbx ready "$BRANCH"
eval "$(sbx env "$BRANCH")"
./run-tests.sh
```

`sbx with` exits with the command's status; `--keep` leaves the sandbox after a failure.
→ [TROUBLESHOOTING.md](TROUBLESHOOTING.md#on-a-shared-or-persistent-ci-runner)

---

## 4 · Seed once, fork many

**Who:** agents or test runs that each need the same seeded data. The expensive part is loading the
data, not the container. Pay for it once.

Seed in the spec, so the golden state is reproducible:

```json
{ "postgres": { "image": "postgres:16-alpine", "ports": [5432],
                "env": { "POSTGRES_USER": "app", "POSTGRES_PASSWORD": "app", "POSTGRES_DB": "app" },
                "health": "psql -U app -d app -c 'select 1'",
                "files": { "./schema.sql": "/tmp/schema.sql" },
                "init":  [ "psql -U app -d app -f /tmp/schema.sql" ] } }
```

```sh
sbx create main                # seeded by init, once
sbx snapshot main golden
sbx fork golden agent-1        # its own copy and ports
sbx fork golden agent-2        # a write in one is invisible to the others
```

Or seed an existing sandbox by hand:

```sh
sbx cp   main postgres ./schema.sql :/tmp/schema.sql
sbx exec main postgres psql -U app -d app -f /tmp/schema.sql
```

A snapshot saves filesystems, not memory: forks start cold against warm data. It does not pause
the service, so stop writes first if it must be exact.
→ [TROUBLESHOOTING.md](TROUBLESHOOTING.md#a-fork-is-missing-the-write-i-just-made)

---

## 5 · Park a process and resume it

**Who:** anyone who needs the *process* back, not just its data: a REPL's variables, a warm cache.
A normal wake starts the service cold against its disk.

```sh
sbx checkpoint agent-42 mid-thought    # save memory + processes, and freeze
sbx resume     agent-42 mid-thought    # bring them back as they were
```

Needs CRIU on a Linux host, and is verified on a **podman** runtime. Refused on macOS. For
memory kept across ordinary sleeps, set `"on_idle": "freeze"` ([SPEC.md](SPEC.md#on_idle-freeze-keeps-memory-instead)).

---

## 6 · A preview link for somebody else

**Who:** someone sharing a branch preview with a reviewer who cannot reach their laptop.

```sh
sbx url my-branch web                              # https://....trycloudflare.com
sbx url my-branch web --via ngrok                  # or cloudflared, ssh
```

The tunnel points at the public port, so the sandbox sleeps until somebody opens the link. A
browser waiting on a cold wake sees a blank page; turn on a "starting" page for waits over a
second:

```sh
SBX_FEATURES=waiting-page sbx serve --idle 5m &
```

Non-HTTP traffic (postgres, redis, TLS) is never answered by that page. A full per-PR workflow is
in [examples/pr-preview](../examples/pr-preview/).

---

## 7 · A browser that sleeps

**Who:** scrape jobs and Playwright/Puppeteer tests. A headless Chrome is a container that speaks
TCP, so it sleeps and wakes like everything else.

```sh
sbx create my-branch --template browser
eval "$(sbx env my-branch)"
curl "http://$CDP_HOST:$CDP_PORT/json/version"     # wakes it; then drive it over CDP
```

Measured wake: median 3.7 s cold, 0.77 s warm (n=5, macOS arm64, v0.1.0, [BENCHMARKS.md](BENCHMARKS.md#a-heavier-workload-headless-chrome)).
That is Chrome's own startup, not sbx's. → [examples/browser](../examples/browser/)

---

## 8 · A sandbox that is not on your laptop

**Who:** a laptop that cannot run the stack, or an agent you want off your machine. Deploy each
service to a platform you already use, and talk to it as if it were local.

```sh
sbx pack --spec sandbox.json          # one build context per service, under sbx-pack/
# deploy sbx-pack/db/ and sbx-pack/cache/, each with SBX_CONNECT_TOKEN set

SBX_CONNECT_TOKEN_DB=... SBX_CONNECT_TOKEN_CACHE=... \
  sbx connect db=https://db.example.dev cache=https://cache.example.dev
#   db     ->  127.0.0.1:5432
#   cache  ->  127.0.0.1:6379
```

Watch and control the deployments from one dashboard (wake, sleep, `L` limit, `d` remove,
`l` logs, `f` forward ports):

```sh
sbx ui --connect db=https://db.example.dev --connect cache=https://cache.example.dev
```

`--front` reaches something the container can route to and you cannot, such as a managed
database on a private network:

```sh
sbx serve --connect-addr=":$PORT" --behind-proxy --front db=10.0.4.7:3306
```

- **The token is the whole of the security.** With `--front HOST:PORT` it gates everything that
  container can reach on those ports. → [SECURITY.md](../SECURITY.md)
- **No volume means no data** on most platforms, which replace containers.
- **Every round trip crosses the internet.** A chatty test suite will notice.

---

## 9 · A box to run your own commands in

**Who:** anyone who wants to build, test or edit *inside* the sandbox. A service image (postgres,
redis) has no git or toolchain, so declare a service that holds your tools and mounts your source:

```json
{
  "version": 1,
  "services": {
    "dev": {
      "image": "golang:1.26-alpine",
      "ports": [7777],
      "args": ["sleep", "infinity"],
      "mounts": { ".": "/work" }
    }
  }
}
```

```sh
sbx create my-branch
sbx exec -t my-branch dev sh          # a shell in /work with your code
sbx exec my-branch dev go test ./...
```

- `mounts` `"."` is the spec's directory; writes go both ways. On macOS and Windows it must be a
  path the VM shares (under your home directory).
- `args` keeps the container alive. `ports` is required but unused.
- `sbx exec` wakes a sleeping box first.

**From an editor (preview).** An ssh connection is an ordinary TCP dial, so it wakes the box; VS
Code Remote-SSH, JetBrains Gateway, `scp` and `rsync` all work. Use an image that runs sshd:

```sh
SBX_FEATURES=ssh sbx ssh feature-x --user dev
# prints:
#   ssh -p 20060 dev@127.0.0.1
#   code --remote ssh-remote+dev@127.0.0.1:20060 /work
```

An attached editor keeps the box awake (VS Code pings every five seconds); close it and it sleeps.

**From a devcontainer (preview).** Import `.devcontainer/devcontainer.json` as a starting spec.
What cannot be translated (features, `postStartCommand`, compose files) is listed on stderr.

```sh
SBX_FEATURES=devcontainer sbx init --from-devcontainer . > sandbox.json
```

---

## 10 · An agent box that only calls out

**Who:** an agent that works *inside* a box and is never dialled. sbx measures idleness on inbound
bytes, so such a box looks idle and sleeps mid-task. Declare an allow-list: calls out go through
sbx's proxy, reach only those hosts, and count as activity.

```json
{
  "version": 1,
  "services": {
    "agent": {
      "image": "python:3.12",
      "ports": [7777],
      "egress_allow": ["api.anthropic.com", "pypi.org", "github.com"],
      "idle": "10m"
    }
  }
}
```

The box stays awake while calling out and sleeps ten minutes after it stops. Tighten a running
box with `sbx egress agent --deny '*.pastebin.com'`. Without an allow-list, or for non-HTTP
traffic, use `"idle": "never"`.
→ [SPEC.md](SPEC.md#egress_allow-is-a-domain-allow-list)

---

## 11 · The same spec in a cluster

**Who:** teams that want per-branch services in Kubernetes rather than on laptops. The spec does
not change.

```sh
sbx create my-branch --provider kubernetes --namespace sbx
sbx env    my-branch --provider kubernetes
```

Services become Deployments and Services; `exports` point at cluster-internal addresses.
[`deploy/`](../deploy/) has the activator that plays the daemon's part in the cluster. Some fields
are refused rather than half-applied (`build`, `mounts`, `cap_add`, all egress settings), and
`sbx url` points you at an Ingress. → [SPEC.md](SPEC.md#provider-support)

---

## 12 · Untrusted code in a microVM

**Who:** anyone running code they did not write: agent output, user submissions. A container shares
the host kernel. A Firecracker microVM has its own kernel, and its VMM runs jailed.

```sh
sbx fc backend                                  # can this machine run microVMs, and how?
sbx serve --provider firecracker --idle 5m &
sbx create untrusted --spec sandbox.json --provider firecracker
```

On Linux with `/dev/kvm` it runs directly. On an M3+ Mac or Windows 11 it runs in a helper VM,
started on demand. Some spec fields are refused (`build`, `files`, `init`, `mounts`); the image
must run as root. → [SPEC.md](SPEC.md#the-same-spec-as-microvms)

Lighter options on docker or Kubernetes: `--isolation gvisor` or `--isolation kata`, refused with a
reason when the runtime is absent.

---

## 13 · Existing OpenSandbox SDK code

**Who:** teams with code written for OpenSandbox's SDKs who want to run it locally, with no
account. sbx serves the same lifecycle API, and its SDKs work unchanged.

```sh
sbx serve --osb-addr 127.0.0.1:8080 &
export OPEN_SANDBOX_DOMAIN=127.0.0.1:8080 OPEN_SANDBOX_PROTOCOL=http
export OPEN_SANDBOX_API_KEY="$(cat ~/.sbx/osb/key)"
python my_agent.py
```

Add `--osb-pool python:3.11-slim` for creates answered in milliseconds. The same API backs
`sbx mcp` for MCP clients. → [AI-AGENTS.md](AI-AGENTS.md#from-opensandbox-sdk-code)
