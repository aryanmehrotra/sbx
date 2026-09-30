# sandbox.json

Reference for `sandbox.json`, the file in your repo that lists the services a branch needs. It
says what runs and how to tell it is ready. `sbx serve` decides when it starts and stops.

```sh
sbx init > sandbox.json     # a working starting point
sbx validate                # check it without creating anything; no docker needed
```

To skip the file, use a built-in template, one of the [`examples/`](../examples/) specs:

```sh
sbx templates                     # analytics browser nginx postgres web-stack
sbx create my-site --template nginx
```

For tasks built on these fields (seeding, CI, agents, microVMs), see [GUIDES.md](GUIDES.md).

## Top level

| field | required | meaning |
|---|---|---|
| `version` | yes | Always `1` |
| `services` | yes | Services, keyed by name |
| `exports` | | Variable name → `service:port`, e.g. `"DATABASE_PORT": "postgres:5432"` |
| `health_interval` | | Default probe interval for every service. Default `300ms` |

## Per service

| field | type | default | meaning |
|---|---|---|---|
| `image` | string | | Image to run. Exactly one of `image` or `build` |
| `build` | object | | `{"context": "./app", "dockerfile": "Dockerfile"}` |
| `ports` | int list | required | Container-side ports. Public ports are assigned |
| `health` | string | none | Command run inside the container. Ready when it exits 0 |
| `health_interval` | duration | `300ms` | How often `health` runs. 50ms to 5m |
| `entrypoint` | string list | image's | Replaces the image's ENTRYPOINT |
| `args` | string list | image's | Replaces the image's CMD |
| `env` | map | | Environment variables. Values may use `${VAR}` |
| `volume` | path | none | One container path kept across sleeps |
| `mounts` | map | | Host dir → container path, read-write. Docker only |
| `files` | map | | Host file → container path, read-only. Relative to the spec |
| `init` | string list | | Run once after the first healthy check, not on each wake. Schemas, seed data |
| `depends_on` | string list | | Services that must be ready first, at create and on every wake |
| `optional` | bool | `false` | Created only with `--optional`. Still reserves its ports |
| `idle` | string | daemon's `--idle` | `"30m"`, or `"never"` / `"0"` to never sleep. Checked every second, whatever the window: a service is stopped within about a second of its window running out, however slow other stops are. On docker a workload that ignores SIGTERM then takes up to 10 s more to exit |
| `on_idle` | string | `"stop"` | `"freeze"` pauses instead of stopping |
| `egress` | string | open | `"deny"` or `"allow"` (open, through the filter) |
| `egress_allow` | string list | | Reach only these hosts and their subdomains, on ports 80 and 443. `host:port` adds that port |
| `egress_policy` | object | | OpenSandbox network policy. Changeable live with `sbx egress` |
| `cpu` | string | unlimited | Cores: `"0.5"`, `"2"` |
| `memory` | string | unlimited | Cap: `"512m"`, `"2g"` |
| `cap_add` | string list | | Capabilities by name: `["SYS_PTRACE"]`. Not `ALL`. Docker only |
| `gpus` | string | none | Passed to the runtime: `"all"`, `"1"`, `"device=0"` |

Use only one of `egress`, `egress_allow` and `egress_policy`. A spec naming two is refused.

There is no field for a named volume. `readonly_volumes` and `volume_mounts` are refused as
unknown; only the OpenSandbox API attaches volumes.

## Example

```json
{
  "version": 1,
  "services": {
    "postgres": {
      "image": "postgres:16-alpine",
      "ports": [5432],
      "health": "psql -U app -d app -c 'select 1'",
      "env": { "POSTGRES_USER": "app", "POSTGRES_PASSWORD": "app", "POSTGRES_DB": "app" },
      "volume": "/var/lib/postgresql/data",
      "init": ["psql -U app -d app -c 'CREATE TABLE IF NOT EXISTS todo (id serial)'"]
    },
    "redis": {
      "image": "redis:7-alpine",
      "ports": [6379],
      "health": "redis-cli ping"
    },
    "clickhouse": {
      "image": "clickhouse/clickhouse-server:24.3-alpine",
      "ports": [8123],
      "health": "wget -qO- localhost:8123/ping",
      "files": { "clickhouse-low-mem.xml": "/etc/clickhouse-server/config.d/low-mem.xml" },
      "optional": true
    }
  },
  "exports": { "DATABASE_PORT": "postgres:5432", "REDIS_PORT": "redis:6379" }
}
```

For Postgres with `init`, use the `psql ... select 1` health check. `pg_isready` says yes before
`POSTGRES_DB` exists, so `init` can run too early. The `web-stack` and `analytics` templates use
`pg_isready` because they have no `init`. Shipped templates pin images by digest
(`scripts/pin-templates.sh`).

## Provider support

Every refusal names the field and the reason.

| field | docker | kubernetes | firecracker |
|---|---|---|---|
| `build` | yes | refused (needs a registry) | refused |
| `mounts`, `cap_add` | yes | refused | refused |
| `init` | yes | yes | refused |
| `files`, `gpus` | yes | ignored, no error | refused |
| `egress`, `egress_allow`, `egress_policy` | yes | refused | yes (unset means `deny`) |
| `on_idle: "freeze"` | yes | refused | yes |
| everything else | yes | yes | yes |

## On `--provider firecracker`

The same file runs each service in a Firecracker microVM: a small VM with its own Linux kernel.
What changes:

| field | on firecracker |
|---|---|
| `image` | Booted as the VM's root, read-only and shared, plus a writable layer per VM (`SBX_FC_DISK_SIZE`, default 10g) |
| `cpu` | Whole vCPUs, rounded up to 1 or an even number: `"0.5"` is 1, `"3"` is 4. Default 1 |
| `memory` | Guest RAM, default `256m`, and the size of its sleep snapshot. Fixed at create |
| `health` | Run in the guest via `/bin/sh -c`, 5 s timeout. Without it, the first accepting port means ready |
| `volume` | Not needed: the VM's disk persists across sleep |
| `egress` | Unset means `deny`. Private ranges stay closed unless `sbx serve --vm-egress-allow` opens them |
| image `USER` | Must be root. A non-root image is refused |

A snapshot from `sbx snapshot` restores that VM, memory included, as the same sandbox only.
Environment knobs are in [CLI.md](CLI.md#firecracker-microvms).

Where microVMs run:

| host | backend |
|---|---|
| Linux with `/dev/kvm` | direct |
| macOS 15+, Apple M3+ | helper Linux VM `sbx-fc` (lima, else colima; `SBX_FC_VM_DRIVER=colima` picks colima) |
| Windows 11 | WSL2 distro `sbx-fc` |
| Kubernetes | `--isolation firecracker` → RuntimeClass `kata-fc` |
| anything else | refused, with the reason and the fix |

Through a helper VM, `sbx env` prints the same host ports and a connection wakes the microVM.
`sbx fc backend` and `sbx doctor` say which row you are on. What is tested where:
[platform status](ARCHITECTURE.md#platform-status).

## Field notes

### Exports

Each `*_PORT` export also sets a matching host variable. `DATABASE_PORT` gives `DATABASE_HOST`.
`PGPORT` gives `PGHOST`, so `psql -U app -d app` needs no host or port. A bare `PORT` gets none.

Each sandbox gets its own block of public ports, so two sandboxes can both run Postgres. Exports
give those ports the names your scripts expect, like `{"DB_PORT": "mysql:3306"}`. Read them with
`sbx env`.

### `image` or `build`

```json
{ "build": { "context": "./app" }, "ports": [3000] }
```

`context` is relative to the spec file, `dockerfile` to the context. The image tag is a hash of
the context, so an unchanged context is a cache hit. The hash ignores timestamps, `.git` and
`node_modules`, and includes file modes.
[Why](DECISIONS.md#a-built-image-is-keyed-by-its-content-never-by-its-age).

### `health` is close to required

Without it the daemon can only dial the published port, which Docker answers before the server
inside is up. It then waits a flat 2 s per wake.

The command must exist in the image. A Chrome image with no `wget` cannot be checked with `wget`,
and it looks like a service that never starts. Check first:

```sh
docker run --rm --entrypoint sh <image> -c 'command -v wget curl'
```

### `health_interval` is what those probes cost

The probe runs inside the container whether or not anyone is waiting. Fourteen services at
300 ms is about 47 container commands a second. Set it once at the top level and override per
service.

It is also the floor on how long a wake appears to take: a service ready in 40 ms reports 300 ms
at the default. On Kubernetes it becomes `periodSeconds`, rounded up to whole seconds.

### `depends_on`

```json
{ "api":      { "build": { "context": "." }, "ports": [3000], "depends_on": ["postgres"] },
  "postgres": { "image": "postgres:16-alpine", "ports": [5432],
                "health": "psql -U app -d app -c 'select 1'" } }
```

Without it, services are created in alphabetical order. A connection to `api` wakes `postgres`
first; independent services wake in parallel. Port numbering stays alphabetical. A missing
service or a cycle is refused.

### `entrypoint` and `args`

`args` alone replaces CMD, which the image's ENTRYPOINT then receives. Use `entrypoint` to run a
different program: `"entrypoint": ["python", "-m"], "args": ["http.server", "8000"]`.

### `${VAR}` in `env`

```json
{ "env": { "POSTGRES_PASSWORD": "${DB_PASSWORD}" } }
```

Keeps a secret out of a committed file. Works in `env` values only. Any other `${...}` form, such
as a default (`${VAR:-x}`) or nesting, is refused at load, every one in the file in one error.
Write `$${` for a literal `${`: `"$${HOME}"` reaches the container as `${HOME}`. A bare `$NAME` or
`$$` is left alone. An unset variable is an error before anything is created, listing every missing
name in the same error as any refused form. Only commands that start a service need it:
`sbx env` prints ports without it.

### Which spec a sandbox uses

`create` records its spec under `~/.sbx/origins/`. Later commands, snapshots and forks reuse it,
so `sbx env agent-1` needs no flag. An explicit `--spec` or `--template` wins. With no record,
sbx falls back to `./sandbox.json`.

### `cpu` and `memory`

Docker gets `--cpus` and `--memory`. Kubernetes gets `resources.limits`. Set them when you run
many sandboxes on one laptop. Load refuses a value no provider takes, like `"lots"` or `"-1"`, and
a `gpus` value docker would refuse. Each provider still checks its own spelling at create.

### `cap_add`

Name only what the workload needs. sbx has no `privileged` option, so `ALL` is refused: list the
capabilities instead. Names are checked at load against the kernel's list, so `sbx validate`
catches a typo or a blank entry. Case and a `CAP_` prefix do not matter: `SYS_PTRACE` and
`CAP_SYS_PTRACE` are the same. Docker's default seccomp profile still applies, so CRIU (a process-checkpoint
tool) fails inside a sandbox; run `sbx checkpoint` on the host instead. Kubernetes refuses
`cap_add` because Pod Security admission decides capabilities there.

### `idle` keeps a sandbox awake while it works

```json
{ "image": "ubuntu:24.04", "ports": [7777], "idle": "never" }
```

sbx sleeps a service when no traffic crosses its ports for the idle window. Work inside the
sandbox, like a long build, sends none. `"never"` keeps it awake until you sleep or remove it. A
service with `egress_allow` needs this less, because its calls out count as activity.

### `on_idle: "freeze"` keeps memory instead

```json
{ "image": "python:3.12", "ports": [8888], "on_idle": "freeze" }
```

When idle, the service is paused, not stopped. Memory and processes are kept, it uses no CPU, and
the next connection resumes it without a restart. It holds its memory while asleep. Docker and
Firecracker support it. Sandboxes created through the OpenSandbox API default to it. Timings:
[BENCHMARKS.md](BENCHMARKS.md#sbx-by-itself).

### `optional`

A branch that never uses the analytics store does not run one. Its ports stay reserved, so adding
it later renumbers nothing.

## Egress: the network a service may reach

Egress is traffic going out of a service. Pick one:

| you want | write |
|---|---|
| no way out | `"egress": "deny"` |
| only these hosts | `"egress_allow": ["api.openai.com", "pypi.org"]` |
| rules: hosts, wildcards, CIDRs, a default | `"egress_policy": {...}` |
| open now, narrowed later | `"egress": "allow"` |

Kubernetes refuses all four rather than run a policy nothing enforces. Firecracker supports all
four.

`"deny"` turns off routing out of the sandbox's own network. Ports are still published, so waking
works, and DNS still resolves. Under `--isolation gvisor` it does not: gVisor does not use
docker's DNS server on a sandbox's network, so neither internet names nor service names resolve.
Use addresses there.

`egress_allow` sends clients through a filtering proxy via `HTTP_PROXY` and `HTTPS_PROXY`. A
client that ignores them has no route out. Each entry matches the host and its subdomains, on
ports 80 and 443. Write an entry as `host:port` to add that port for that host:
`"github.com:22"` lets a client tunnel SSH to github.com through the proxy. A port that is not a
number from 1 to 65535 is refused, and so is any port under `--provider firecracker`.

On native Linux Docker the filter runs inside `sbx serve`. On colima, Docker Desktop or rootless
Docker it runs as a small container on the sandbox's network, at a fixed address that services
find through `/etc/hosts`, so it works under `--isolation gvisor` too. Calls out keep every
allow-listed service in the sandbox awake, including during a long streaming response.

`egress_policy` uses the `NetworkPolicy` format of OpenSandbox release-1.1.0:

```json
{ "image": "python:3.12", "ports": [8000],
  "egress_policy": { "defaultAction": "deny",
                     "egress": [ { "action": "allow", "target": "*.pypi.org" },
                                 { "action": "allow", "target": "pypi.org" },
                                 { "action": "deny",  "target": "10.0.0.0/8" } ] } }
```

| rule | behaviour |
|---|---|
| targets | `example.com` is that host. `*.example.com` is subdomains only, not the apex. IPs and CIDRs, v4 or v6 |
| name rules | First match in list order wins |
| address rules | A deny beats an allow, in any order |
| default | `defaultAction`. Omitted means `deny` |
| a hostname | Judged by name, then each address it resolves to |
| refused | A URL or `host:port` as a target |

`egress: "allow"` equals `{"defaultAction":"allow"}`. `egress_allow: ["openai.com"]` equals deny
by default plus `openai.com` and `*.openai.com`. Services in one sandbox share one filter, so they
must declare the same policy. Allow-lists are merged: every service gets the union of them.

Limits:

- Ports 80 and 443 only, for plain HTTP and for `CONNECT`. Any other port gets 403 naming the
  port, including under a default of allow, unless an `egress_allow` entry names it as
  `host:port`. `egress_policy` has no port field.
- Loopback and link-local addresses, including cloud metadata at `169.254.0.0/16`, are refused unless a rule names them.
- On colima and Docker Desktop, the machine behind the filter is refused whatever a rule says: every
  docker network's gateway, the default bridge, and what `host.docker.internal`,
  `host.lima.internal` and `gateway.docker.internal` resolve to (the whole `/24`), and the filter's own
  addresses, `sbx-egress` included. A service cannot reach the VM or your Mac through the proxy.
- A docker network created after a filter started is refused from the next discovery pass of
  `sbx serve` (`--refresh`, 15 s by default), which pushes the engine's gateways to every
  filter. While no `sbx serve` runs, and on a remote docker, a network created later is not
  refused until the sandbox is removed and created again.
- A request to one of those addresses gets 403 saying so, on any port. The port hint ("list
  `host:port` in `egress_allow`") is given only to a host the policy would otherwise let through.
- Traffic between services in the same sandbox is not filtered.

Run `sbx create` again after editing `egress_policy` or `egress_allow` and the filter is replaced
with the new declaration. Live changes made with `sbx egress` are dropped then, because they were
changes to the old declaration. A filter built by an older sbx is replaced too, and there live
changes are kept, since the declaration is the same. Other edits to a service that already exists
still need `sbx rm` first.

### Change a running sandbox's policy

```sh
sbx egress agent-1                                     # what is in force
sbx egress agent-1 --deny '*.pastebin.com' --deny 10.0.0.0/8
sbx egress agent-1 --default deny --allow api.anthropic.com
sbx egress agent-1 --remove 10.0.0.0/8
sbx egress agent-1 --reset                             # back to the spec
sbx egress agent-1 --json                              # OpenSandbox's policy status
```

Nothing restarts. New rules go ahead of existing ones, so a deny can carve into a wildcard allow.
`--remove` names a target exactly as the policy lists it, and one with no rule is an error that
lists the rules there are. Open connections are not cut. The live policy is saved in `~/.sbx/egress/<sandbox>.json`,
survives restarts, and is deleted by `sbx rm`. Only a sandbox created with `egress_policy`,
`egress_allow` or `egress: "allow"` has a filter to change.

A common pattern: start with `"egress": "allow"`, install dependencies, then run
`sbx egress <sandbox> --default deny --allow <api-host>` before untrusted work starts.

## Coming from docker-compose

| docker-compose | sandbox.json | note |
|---|---|---|
| `image` | `image` | pin it |
| `build.context` / `build.dockerfile` | same | tagged by a hash of the context |
| `ports: ["5432:5432"]` | `ports: [5432]` | container side only; the host side is assigned |
| `environment` | `env` | `${VAR}` works in both |
| `entrypoint` | `entrypoint` | |
| `command` | `args` | |
| `volumes: ["pgdata:/var/lib/postgresql/data"]` | `volume: "/var/lib/postgresql/data"` | one per service |
| `volumes: ["./my.conf:/etc/my.conf:ro"]` | `files: {"./my.conf": "/etc/my.conf"}` | read-only |
| `volumes: ["./src:/work"]` | `mounts: {"./src": "/work"}` | read-write, Docker only |
| `healthcheck.test` | `health` | a shell command, run inside the container |
| `depends_on` | `depends_on` | waits for health |
| `deploy.resources.limits` | `cpu`, `memory` | |
| `profiles` | `optional` | created with `--optional` |
| - | `exports` | names the assigned ports for your tools |

`networks` and `restart` do not carry over: each sandbox gets its own network and the daemon owns
the lifecycle. Compose's `pg_isready` check becomes the `psql ... select 1` form shown in the
[example](#example). Run `sbx validate` on the result.
