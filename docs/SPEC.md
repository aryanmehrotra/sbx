# The spec

Reference for `sandbox.json`: the one committed file that says what services a branch needs.
It says what exists, how to tell it is serving, and how to reach it. It never says when to start
or stop; `sbx serve` does that by watching the ports.

`sbx init > sandbox.json` writes a working one. `sbx validate` checks one without creating anything.
For tasks built on these fields (seeding, CI, agents, microVMs), see [GUIDES.md](GUIDES.md).

---

## A complete one

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

**Use `psql ... select 1` as a postgres health check when the service has `init`.** `pg_isready`
answers yes while postgres is still bootstrapping, before `POSTGRES_DB` exists, so `init` can run
too early. The `postgres` template and `sbx init` use the `psql` form. The `web-stack` and
`analytics` templates use `pg_isready -U app -d app` and have no `init`.

The shipped templates also pin each image by digest (`scripts/pin-templates.sh`).

---

## Top level

| field | required | meaning |
|---|---|---|
| `version` | yes | Always `1` |
| `services` | yes | The services, keyed by name |
| `exports` | | Map a variable name to `service:port`, e.g. `"DATABASE_PORT": "postgres:5432"` |
| `health_interval` | | Default probe interval for every service. Default `300ms` |

### Every port export gets a host to go with it

A declared export produces two variables. `DATABASE_PORT` also yields `DATABASE_HOST`. `PGPORT`
yields `PGHOST` (no underscore, as libpq reads it), so `psql -U app -d app` reaches the sandbox
with no host or port argument. `MYSQL_PORT` yields `MYSQL_HOST`.

The rule: strip a trailing `_PORT`, else a trailing `PORT`, and append `_HOST` or `HOST` to
match. A bare `PORT` export gets no host variable.

Use `exports` to keep existing scripts working: `{"DB_PORT": "mysql:3306"}` sets `DB_PORT` to the
public port of mysql's 3306.

---

## Per service

| field | type | default | meaning |
|---|---|---|---|
| `image` | string | | Container image to run. Exactly one of `image` or `build` |
| `build` | object | | Build instead: `{"context": "./app", "dockerfile": "Dockerfile"}` |
| `ports` | int list | required | Container-side ports. Public ports are assigned, never chosen |
| `health` | string | none | Command run **inside** the container; passes when it exits 0 |
| `health_interval` | duration | `300ms` | How often `health` runs. 50ms to 5m |
| `entrypoint` | string list | image's | Replaces the image's ENTRYPOINT. `args` follow it |
| `args` | string list | image's | Replaces the image's CMD (appended to the entrypoint) |
| `env` | map | | Environment variables. Values may use `${VAR}` |
| `volume` | path | none | One container path to persist across sleeps |
| `mounts` | map | | Host directory → container path, read-write. Docker only |
| `files` | map | | Host file → container path, read-only. Paths relative to the spec |
| `init` | string list | | Commands run **once**, after the first healthy check |
| `depends_on` | string list | | Services that must serve first, at create and on every wake |
| `optional` | bool | `false` | Created only with `--optional`. Still reserves its ports |
| `idle` | string | daemon's `--idle` | `"30m"`, or `"never"` / `"0"` to never sleep |
| `on_idle` | string | `"stop"` | `"freeze"` pauses instead: memory and processes kept, resumed without a restart ([measured](BENCHMARKS.md#freeze-and-thaw-v0140)) |
| `egress` | string | open | `"deny"`: no routed egress. `"allow"`: open, but through the filter |
| `egress_allow` | string list | | Reach only these hosts and their subdomains |
| `egress_policy` | object | | A network policy in OpenSandbox's format. Changeable live with `sbx egress` |
| `cpu` | string | unlimited | Cores: `"0.5"`, `"2"` |
| `memory` | string | unlimited | Cap: `"512m"`, `"2g"` |
| `cap_add` | string list | | Linux capabilities without `CAP_`: `["SYS_PTRACE"]`. Docker only |
| `gpus` | string | none | Passed to the runtime: `"all"`, `"1"`, `"device=0"` |

`egress`, `egress_allow` and `egress_policy` are alternatives; a spec naming two is refused. See
[Egress](#egress-the-network-a-service-may-reach).

### Provider support

Every refusal names the field and the reason.

| field | docker | kubernetes | firecracker |
|---|---|---|---|
| `build` | yes | refused (needs a registry) | refused |
| `mounts`, `cap_add` | yes | refused | refused |
| `init` | yes | yes | refused |
| `files`, `gpus` | yes | not applied | refused |
| `egress`, `egress_allow`, `egress_policy` | yes | refused | yes (unset means `deny`) |
| `on_idle: "freeze"` | yes | refused | yes |
| everything else | yes | yes | yes |

On Kubernetes, `files` and `gpus` are currently not applied to the pod and raise no error.

### On `--provider firecracker`

The same file, with a microVM underneath: a small virtual machine with its own Linux kernel, run
by Firecracker (see [GUIDES.md](GUIDES.md#stronger-isolation-with-microvms)). What changes:

| field | on firecracker |
|---|---|
| `image` | Booted as the VM's root: shared read-only, with a writable layer per VM (`SBX_FC_DISK_SIZE`, default 10g) |
| `cpu` | Whole vCPUs, rounded up to 1 or an even number: `"0.5"` is 1, `"3"` is 4. Default 1 |
| `memory` | Guest RAM, default `256m`, and the size of its sleep snapshot. Fixed at create |
| `health` | Run in the guest via `/bin/sh -c`, 5 s timeout. None: the first accepting port means ready |
| `volume` | Nothing extra: the VM's disk is its own and persists across sleep |
| `egress` | Unset means `deny` (no NAT). `allow`, `egress_allow`, `egress_policy` go through the filter. Private ranges stay closed unless `sbx serve --vm-egress-allow` opens them |
| the image's `USER` | Must be root. A non-root image is refused |

A snapshot name from `sbx snapshot` restores that VM, memory included, as the same sandbox and
service only. Where a microVM runs (Linux, Mac, Windows, cluster) is in
[The same spec as microVMs](#the-same-spec-as-microvms). Environment knobs are in [CLI.md](CLI.md#firecracker-microvms).

There is no field for mounting an arbitrary named volume. `readonly_volumes` and `volume_mounts`
are refused as unknown fields; only the OpenSandbox API attaches volumes, after its own checks.

---

## Field notes

### `image` or `build` - exactly one

```json
{ "build": { "context": "./app" }, "ports": [3000] }
```

`context` is relative to the spec file. `dockerfile` defaults to `Dockerfile`, relative to the
context. Giving both `image` and `build` is an error.

**The tag is a hash of the context**, so an unchanged context is a cache hit:

```
$ sbx create feat-x            # first time
  web          building...
$ sbx create feat-y            # same context
  web          build cached (sbx-build-bc02342a9ba51b10)
```

| | |
|---|---|
| **timestamps - out** | a fresh `git clone` rewrites every mtime |
| **file modes - in** | a script that loses its executable bit is a different image |
| **`.git`, `node_modules` - out** | otherwise every commit and install busts the cache |

→ [DECISIONS.md](DECISIONS.md#a-built-image-is-keyed-by-its-content-never-by-its-age)

### Ports are assigned

Each sandbox gets its own block of public ports, so two sandboxes can both run "a postgres".
The container side is the port you declare; the public side is assigned. Read them with `sbx env`; `exports` gives them the names your tools expect.

### `health` is close to required

Without it the daemon can only dial the published port, and Docker answers that before the server
inside does. It then waits a flat 2 s per wake. → [DECISIONS.md](DECISIONS.md#a-published-port-is-not-readiness)

**The health command must exist in the image.** A Chrome image with no `wget` cannot be checked
with `wget`, and the failure looks like a service that never starts. Check first:

```sh
docker run --rm --entrypoint sh <image> -c 'command -v wget curl'
```

### `health_interval` is what those probes cost

The probe runs inside the container whether or not anybody is waiting. Fourteen services at
300 ms is about 47 container commands a second. Set it sandbox-wide and override per service:

```json
{ "version": 1,
  "health_interval": "1s",
  "services": { "db": { "image": "postgres:16-alpine", "ports": [5432],
                        "health": "psql -U app -d app -c 'select 1'",
                        "health_interval": "300ms" } } }
```

It is also the floor on how long a wake appears to take: a service ready in 40 ms reports as
300 ms at the default. On Kubernetes it becomes `periodSeconds`, rounded up to whole seconds.

### `depends_on` orders creation, and waking

```json
{ "api":      { "build": { "context": "." }, "ports": [3000], "depends_on": ["postgres"] },
  "postgres": { "image": "postgres:16-alpine", "ports": [5432],
                "health": "psql -U app -d app -c 'select 1'" } }
```

Without it, services are created alphabetically, so `api` would start before `postgres`.

- **It orders wakes.** A connection to `api` wakes `postgres` first. Independent services wake in
  parallel.
- **It does not change ports.** Ordinals stay alphabetical.
- A dependency on an undeclared service, or a cycle, is refused.

### `entrypoint` replaces the program

```json
{ "image": "python:3.12", "ports": [8000],
  "entrypoint": ["python", "-m"], "args": ["http.server", "8000"] }
```

`args` alone replaces only CMD, which the image's ENTRYPOINT then receives. Use `entrypoint` to
run a different program.

### `${VAR}` keeps a secret out of a committed file

```json
{ "env": { "POSTGRES_PASSWORD": "${DB_PASSWORD}" } }
```

- **`env` values only.** Not images, health commands or init steps.
- **No defaults or nesting.** No `${VAR:-fallback}`.
- **An unset variable is an error**, reported before anything is created, listing every missing name.
- **`${...}` only.** A bare `$NAME` is left alone.

### sbx remembers which spec a sandbox came from

`create` records the spec under `~/.sbx/origins/`, and later commands use it when you pass none:

```sh
sbx create main --template postgres
sbx env    main                  # no flag needed
sbx snapshot main golden         # the snapshot inherits it
sbx fork   golden agent-1        # and so does the fork
sbx env    agent-1               # still no flag
```

An explicit `--spec` or `--template` always wins. A missing record, or one naming a deleted file,
falls back to `./sandbox.json`.

### Check it without creating it

```sh
sbx validate                    # ./sandbox.json
sbx validate path/to/spec.json
```

Runs the same loader as `create`, needs no docker daemon, and warns about things like a service
with no `health`. Good for a pre-commit hook or a lint job.

### `init` runs once, not on every wake

Schemas, users, seed data. A woken container already has whatever `init` created.

### `cpu` and `memory` are the ceiling a laptop needs

```json
{ "image": "postgres:16-alpine", "ports": [5432], "cpu": "0.5", "memory": "512m" }
```

Docker gets `--cpus`/`--memory`; a cluster gets `resources.limits` (requests are left alone).
Set them when you run many sandboxes.

### `cap_add` grants a capability, and is not `privileged`

```json
{ "image": "golang:1.26", "ports": [7777], "cap_add": ["SYS_PTRACE"] }
```

Name only what the workload needs. sbx does not offer `privileged`, which turns off seccomp and
AppArmor (the kernel filters that limit what a container may do) and hands over the host's
devices. Docker's default seccomp profile still applies, so CRIU (the process-checkpoint tool)
inside a sandbox fails on `mount`; run `sbx checkpoint` on the host instead.

Kubernetes refuses `cap_add`: Pod Security admission, not the manifest, decides capabilities.

### `idle` keeps a sandbox awake while it works

```json
{ "image": "ubuntu:24.04", "ports": [7777], "idle": "never" }
```

sbx sleeps a service after the idle window with no traffic through its port. Work *inside* the
sandbox (a long command, a compute loop) sends none. `"never"` (or `"0"`) keeps it awake until you
sleep or remove it; `"30m"` sets a longer window. It still wakes on a connection as usual.

A service with `egress_allow` needs this less: its calls out count as activity. See
[below](#egress_allow-is-a-domain-allow-list).

### `on_idle: "freeze"` keeps memory instead

```json
{ "image": "python:3.12", "ports": [8888], "on_idle": "freeze" }
```

When idle, the service is paused rather than stopped: memory and running processes are kept, no
CPU is used, and the next connection resumes it without a restart
([measured](BENCHMARKS.md#freeze-and-thaw-v0140)). It holds its memory while asleep.
Docker and firecracker support it; Kubernetes refuses it. Sandboxes created through the
OpenSandbox API default to `freeze`.

### `optional` still reserves its ports

A branch that never queries the analytics store does not pay for one. Its ports stay reserved, so
adding it later does not renumber anything. → [DECISIONS.md](DECISIONS.md#optional-services-still-reserve-their-ports)

---

## Egress: the network a service may reach

Egress is traffic going *out* of a service, to the internet or your network. Four settings, from
strictest to most flexible:

| you want | write |
|---|---|
| no way out | `"egress": "deny"` |
| only these hosts | `"egress_allow": ["api.openai.com", "pypi.org"]` |
| rules: hosts, wildcards, CIDRs, a default | `"egress_policy": {...}` |
| open now, narrowed later | `"egress": "allow"` |

Kubernetes refuses all four rather than start a service whose policy nothing enforces.
Firecracker supports all four; see [On `--provider firecracker`](#on---provider-firecracker).

### `egress: "deny"` blocks the way out, not the way in

```json
{ "image": "node:22", "ports": [3000], "egress": "deny" }
```

Docker gets a per-sandbox bridge with IP masquerade off: nothing routed leaves, but ports are still
published, so waking works. DNS still resolves.

### `egress_allow` is a domain allow-list

```json
{ "image": "python:3.12", "ports": [8000], "egress_allow": ["api.openai.com", "pypi.org"] }
```

The service reaches only the listed hosts. Each entry matches the host and its subdomains, so
`openai.com` permits `api.openai.com`. `HTTP_PROXY`/`HTTPS_PROXY` point clients at a filtering
proxy; a client that ignores them has no route out at all.

- **Where the filter runs.** Inside `sbx serve` on native Linux docker. On a VM-backed docker
  (colima, Docker Desktop, rootless) it runs as a small container on the sandbox's bridge.
- **Calls out count as activity.** The service stays awake while calling out and sleeps on its timer
  once it stops. Activity is stamped on bytes, so a streaming response keeps it awake.
- **What the stamp reaches.** Every service on the sandbox's bridge that declared its own
  allow-list. A service without one sleeps on its own timer. Two allow-listed services in one
  sandbox keep each other awake.

### `egress_policy` is a network policy, and it changes while the sandbox runs

```json
{ "image": "python:3.12", "ports": [8000],
  "egress_policy": { "defaultAction": "deny",
                     "egress": [ { "action": "allow", "target": "*.pypi.org" },
                                 { "action": "allow", "target": "pypi.org" },
                                 { "action": "deny",  "target": "10.0.0.0/8" } ] } }
```

The shape and semantics are the `NetworkPolicy` of OpenSandbox (an open-source sandbox API that
sbx also serves), at release-1.1.0:

| | |
|---|---|
| **targets** | `example.com` is that host. `*.example.com` is every subdomain, **not** the apex. IPs and CIDRs, v4 or v6 |
| **name rules** | first match in list order wins |
| **address rules** | a deny beats an allow, whatever the order |
| **default** | `defaultAction` for anything unmatched. Omitted means `deny` |
| **a hostname** | judged by name, then every address it resolves to against the address rules |
| **refused** | a URL or `host:port` as a target |

`egress: "allow"` equals `{"defaultAction":"allow"}`. `egress_allow: ["openai.com"]` equals
deny-by-default plus `openai.com` and `*.openai.com`. Two services in one sandbox must declare the
same policy, since they share one filter.

**Limits.** The filter carries HTTP and HTTPS only, so a default-allow service has no raw TCP out
(`git://`, SSH, a remote database). The filter refuses its own loopback and link-local addresses
(including `169.254.0.0/16`, cloud metadata) unless a rule names them. Traffic to sibling services
on the sandbox's own bridge is not egress and is not filtered.

### Change a running sandbox's policy: `sbx egress`

Nothing is recreated or restarted.

```sh
sbx egress agent-1                                     # what is in force
sbx egress agent-1 --deny '*.pastebin.com' --deny 10.0.0.0/8
sbx egress agent-1 --default deny --allow api.anthropic.com
sbx egress agent-1 --remove 10.0.0.0/8
sbx egress agent-1 --reset                             # back to what the spec declared
sbx egress agent-1 --json                              # OpenSandbox's policy status
```

- New rules go **ahead** of existing ones, so a deny can carve into an earlier wildcard allow.
- Each new request is judged by the policy in force; open tunnels are not cut.
- The live policy is saved in `~/.sbx/egress/<sandbox>.json` and survives a daemon restart or
  reboot. `sbx rm` deletes it.
- Only a sandbox created with `egress_policy`, `egress_allow` or `egress: "allow"` has a filter to
  change.

A common pattern: start with `"egress": "allow"`, install dependencies, then lock down with
`sbx egress <sandbox> --default deny --allow <api-host>` before untrusted work starts.

---

## The same spec as microVMs

Nothing in the file names a backend. `--provider firecracker` runs each service in a Firecracker
microVM. Where that runs depends on the host:

| host | backend |
|---|---|
| Linux with `/dev/kvm` | direct |
| macOS 15+, Apple M3+ | helper VM `sbx-fc` (lima, else colima; `SBX_FC_VM_DRIVER=colima` picks colima) |
| Windows 11 | WSL2 distro `sbx-fc` |
| Kubernetes | `--isolation firecracker` → RuntimeClass `kata-fc` |
| anything else | refused, with the reason and the fix |

A helper VM is a small Linux VM that sbx starts on demand, because microVMs need Linux.
What is tested on each host: [README platform status](../README.md#platform-status).

Through a helper VM, `sbx env` prints the same ports on the host, and a connection wakes the
microVM as it would a container. `sbx fc backend` and `sbx doctor` say which row you are on.
→ [DECISIONS.md](DECISIONS.md#a-microvm-off-linux-runs-in-a-helper-vm-not-on-virtualizationframework)

---

## Or skip the file entirely

```sh
sbx templates                             # analytics browser nginx postgres web-stack
sbx create my-site --template nginx
```

The templates are the [`examples/`](../examples/), embedded in the binary.

---

## Coming from docker-compose

A compose service maps almost field for field:

| docker-compose | sandbox.json | note |
|---|---|---|
| `image` | `image` | pin it |
| `build.context` / `build.dockerfile` | `build.context` / `build.dockerfile` | tagged by a hash of the context |
| `ports: ["5432:5432"]` | `ports: [5432]` | **container side only**; the host side is assigned |
| `environment` | `env` | `${VAR}` works in both |
| `entrypoint` | `entrypoint` | |
| `command` | `args` | |
| `volumes: ["pgdata:/var/lib/postgresql/data"]` | `volume: "/var/lib/postgresql/data"` | one per service |
| `volumes: ["./my.conf:/etc/my.conf:ro"]` | `files: {"./my.conf": "/etc/my.conf"}` | read-only, relative to the spec |
| `volumes: ["./src:/work"]` | `mounts: {"./src": "/work"}` | read-write, docker only |
| `healthcheck.test` | `health` | a shell command, run inside the container |
| `depends_on` | `depends_on` | waits for health (`service_healthy`) |
| `deploy.resources.limits` | `cpu`, `memory` | |
| `profiles` | `optional` | created with `--optional` |
| - | `exports` | names the assigned ports for your tools |

A two-service compose file:

```yaml
services:
  db:
    image: postgres:16-alpine
    ports: ["5432:5432"]
    environment: { POSTGRES_USER: app, POSTGRES_PASSWORD: app, POSTGRES_DB: app }
    volumes: [ "pgdata:/var/lib/postgresql/data" ]
    healthcheck: { test: ["CMD-SHELL", "pg_isready -U app"] }
  cache:
    image: redis:7-alpine
    ports: ["6379:6379"]
```

becomes:

```json
{
  "version": 1,
  "services": {
    "db": {
      "image": "postgres:16-alpine",
      "ports": [5432],
      "env": { "POSTGRES_USER": "app", "POSTGRES_PASSWORD": "app", "POSTGRES_DB": "app" },
      "volume": "/var/lib/postgresql/data",
      "health": "psql -U app -d app -c 'select 1'"
    },
    "cache": { "image": "redis:7-alpine", "ports": [6379], "health": "redis-cli ping" }
  },
  "exports": { "DATABASE_PORT": "db:5432", "REDIS_PORT": "cache:6379" }
}
```

Anything reading `DATABASE_PORT` keeps working. Compose's `networks` (each sandbox gets its own)
and `restart` (the daemon owns lifecycle) do not carry over. Run `sbx validate` on the result.
