# The spec

> **Short version:** one committed `sandbox.json` says what services a branch needs. It never
> says when to start or stop them. `sbx init > sandbox.json` gives you a working one;
> `sbx validate` checks it without creating anything.

`sandbox.json` - one file, committed to your repo, describing what a branch needs to run.

It says what exists, how to tell when it is serving, and how to reach it - never when to start or
stop. Lifecycle belongs to `sbx serve`, which watches the ports.

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

**Note the postgres health command:** `psql ... select 1`, not `pg_isready`, which answers yes
while postgres is still bootstrapping - before `POSTGRES_DB` exists - so `init` would run against
a database not there yet. Every bundled template and `sbx init` use the `psql` form.

Images show a readable tag; the shipped templates additionally pin a digest, maintained by
`scripts/pin-templates.sh`.

---

## Top level

| field | required | does |
|---|---|---|
| `version` | ● | `1`. Lets a future format change be detected rather than silently misread |
| `services` | ● | The declared set, keyed by name - a map, so a name is unambiguous when merging |
| `exports` | | Maps port assignments onto the variables your scripts already read - each also yields a matching `_HOST` |

### Every port export gets a host to go with it

A declared export produces two variables. `DATABASE_PORT` also yields `DATABASE_HOST`; `PGPORT`
yields `PGHOST` - no underscore, which is what libpq reads, and why `psql -U app -d app` with no
host or port argument reaches the sandbox. `MYSQL_PORT` yields `MYSQL_HOST` likewise.

The rule: strip a trailing `_PORT` if there is one, otherwise a trailing `PORT`, and append
`_HOST` or `HOST` to match. A bare `PORT` export is left alone rather than becoming `_HOST`.

**`exports` is how adoption stays cheap.** `{"DB_PORT": "mysql:3306"}` becomes
`DB_PORT=<public port of mysql 3306>`. Without it, adopting sbx would mean editing every
script that already knows a port.

---

## Per service

| field | required | does |
|---|---|---|
| `image` | ●* | Any container image |
| `build` | ●* | Build one instead: `{ "context": "./app", "dockerfile": "Dockerfile" }` |
| `ports` | ● | Container-side ports. The public and backing ports are **assigned from the sandbox's slot**, never chosen here |
| `health` | | A command run **inside** the container - how sbx knows it is serving |
| `health_interval` | `300ms` | How often that command runs. Also settable once for the whole sandbox |
| `env` | | Environment variables |
| `args` | | Command arguments, appended to the image's entrypoint |
| `volume` | | One container path to persist. What makes sleeping safe |
| `mounts` | | Host directories bound read-write, `host: /container`. Your disk, visible to both - a source tree, a dump, fixtures a test run leaves behind. Docker only; a cluster refuses, because a hostPath is a node's disk rather than yours |
| `files` | | Read-only host files, mounted; paths are relative to the spec |
| `init` | | Commands run **once**, after the service first reports healthy |
| `depends_on` | | Services that must be serving before this one starts - at creation, and on every wake |
| `optional` | | Not created unless `--optional` - but still reserves its ports |
| `egress` | | `"deny"` - no routed egress. It can still be reached, and can still talk to its own sandbox |
| `egress_allow` | | Reach only these hosts (host or `host:port`, matching subdomains): `["api.openai.com"]`. Everything else is denied, enforced by a filtering proxy |
| `idle` | | Override the idle timer for this service: `"never"` (keep awake while an agent works inside), `"0"`, or a duration like `"30m"` |
| `cpu` | | Cores this service may use: `"0.5"`, `"2"`. Unset means unlimited |
| `memory` | | Memory cap: `"512m"`, `"2g"`. Unset means unlimited |
| `gpus` | | Passed to the runtime verbatim: `"all"`, `"1"`, `"device=0"`. Declared rather than inferred, because a sandbox that quietly takes every GPU on a shared machine is a bad neighbour |

### `image` or `build` - exactly one

\* Every service needs something to run. Give it an `image` to pull, or a `build` to make:

```json
{ "build": { "context": "./app" }, "ports": [3000] }
```

`context` is relative to the spec file; `dockerfile` defaults to `Dockerfile` and is relative
to the context. Both is an error rather than a precedence rule - which wins is exactly what a
reader guesses wrong.

**The tag is a hash of the context**, so an unchanged context is a cache hit and no build runs:

```
$ sbx create feat-x            # first time
  web          building...
$ sbx create feat-y            # same context
  web          build cached (sbx-build-bc02342a9ba51b10)
```

**Content, not a clock.** Change one byte, get a different tag. Change nothing, get the same
tag next month.

Three things are deliberately in or out of the hash:

| | |
|---|---|
| **timestamps - out** | a fresh `git clone` rewrites every mtime, so a time-based key misses on every CI runner, which is exactly where the cache is worth most |
| **file modes - in** | a script that stops being executable is a different image, and a silent cache hit there fails at runtime |
| **`.git`, `node_modules` - out** | otherwise every commit and every install busts the cache |

Why not expire it on a timer? A clock is wrong in both directions - it rebuilds what has not
changed and reuses what has.
→ [DECISIONS.md](DECISIONS.md#a-built-image-is-keyed-by-its-content-never-by-its-age)

Docker only. In a cluster, building means pushing to a registry the nodes can pull from -
credentials sbx has no business assuming - so `sbx create` says so and stops.

### Why ports aren't yours to choose

Two repos that both picked 5432 collide the moment somebody opens both. Each sandbox gets a
slot; ports are assigned from it, and `exports` keeps this invisible to your tooling.

### `health` is close to required

Without it the daemon can only dial the published port - and Docker answers that before the
server inside does, so the first query after a wake lands on a socket about to close.
→ [DECISIONS.md](DECISIONS.md#a-published-port-is-not-readiness)

**The health command must exist in the image.** A Chrome image with no `wget` cannot be
health-checked with `wget`, and the failure looks like a service that never starts. Check
first:

```sh
docker run --rm --entrypoint sh <image> -c 'command -v wget curl'
```

### `health_interval` is what those probes cost

The probe is a command started inside the container, and runs whether or not anybody is
waiting. One service at the default 300 ms is nothing; fourteen is about forty-seven container
commands a second, for ever.

```json
{ "version": 1,
  "health_interval": "1s",
  "services": { "db": { "image": "postgres:16-alpine", "ports": [5432],
                        "health": "pg_isready -U app",
                        "health_interval": "300ms" } } }
```

Reach for the sandbox-wide setting - the cost is a property of the fleet - and override it on a
service where readiness is worth catching quickly or the probe is expensive.

**It is also the floor on how long a wake appears to take.** A wake is not over until the answer
changes, re-evaluated only on this interval, so a service ready in 40 ms reports as 300 ms at the
default and a second at `1s`. Turn it up on a large sandbox; turn it down when measuring wakes.

Below 50 ms and above 5 m are refused: the first spends cpu to learn nothing, the second reports a
service as still waking long after it served.

In a cluster this becomes the readiness probe's `periodSeconds`, which counts whole seconds -
anything under a second becomes one. Zero is not passed through: to the API server zero means
"use my default", which is ten, so a spec asking for a faster probe would have quietly got a
slower one.

### `depends_on` orders creation, and waking

```json
{ "api":      { "build": { "context": "." }, "ports": [3000], "depends_on": ["postgres"] },
  "postgres": { "image": "postgres:16-alpine", "ports": [5432], "health": "pg_isready -U app" } }
```

Without it, services are created alphabetically - so `api` comes up before `postgres`, and an
app that dials its database at boot fails for a reason nowhere in the file.

**It also orders wakes.** A connection to `api` wakes `postgres` first and waits for it, then
starts `api`. Independent siblings wake in parallel, so a layer costs its slowest member
rather than the sum, and a cycle is broken rather than followed.

This used to be excluded, on the reasoning that a service needing another at runtime should
just retry. That does not survive a sleeping peer: a stopped container is not slow to answer,
it is **absent from the network's DNS**, so the dial fails with `no such host` and there is
nothing to retry towards. On a fourteen-service sandbox, six services died that way within a
minute of their datastores being slept.

**Declaring nothing costs nothing.** A service with no `depends_on` takes exactly the path it
always took - which is every single-service sandbox, and the whole of the wake numbers above.

**It does not change ports.** Ordinals stay alphabetical, so adding `depends_on` never moves an
existing sandbox's addresses - a worse bug than the race it fixes.

A dependency on a service the spec does not declare is refused, rather than silently never
applying. So is a cycle.

### `${VAR}` keeps a secret out of a committed file

```json
{ "env": { "POSTGRES_PASSWORD": "${DB_PASSWORD}" } }
```

`sandbox.json` is meant to be committed. `POSTGRES_PASSWORD: "app"` on a throwaway local database
is fine; a private registry credential or a real key some fixture seeding needs would be a secret
in git. A value in `env` may instead reference the environment sbx was invoked with.

Deliberately the smallest version of this:

- **`env` values only.** Not images, not health commands, not init steps - expansion inside a
  command is where this stops being substitution and starts being a shell.
- **No defaults or nesting.** No `${VAR:-fallback}` - your shell already has all of them.
- **An unset variable is an error**, reported before anything is created, listing every missing
  name at once. A database up with an empty password because a variable was not exported is a
  failure that looks like success.
- **`${...}` only** - a bare `$NAME` is left alone, so a password containing a dollar sign
  survives.

Anything further - Vault, 1Password, a cloud secret manager - stays out: a dependency, a
network call, and a credential to fetch the credential, in a binary whose whole claim is that it
has none.

### sbx remembers which spec a sandbox came from

`--template postgres` had to be repeated on `create`, then `env`, then `fork`. Forgetting it
gave one of two things, the second worse: `open sandbox.json: no such file` if the directory had
none, or - if an unrelated `sandbox.json` happened to be there - a clean success against the wrong
spec, since ordinals assigned alphabetically over the declared service names shift and `sbx env`
prints a plausible, wrong port.

So sbx writes it down, under `~/.sbx/origins/`, and uses it when nothing was asked for:

```sh
sbx create main --template postgres
sbx env    main                  # no flag needed
sbx snapshot main golden         # the snapshot inherits it
sbx fork   golden agent-1        # and so does the fork
sbx env    agent-1               # still no flag
```

Always a **default**, never a source of truth: an explicit `--spec` or `--template` wins, a
missing or unreadable record changes nothing, and no command fails because of it. The containers
and their labels remain where a sandbox's truth lives. A recorded path since deleted is ignored
rather than used - chasing a path that no longer exists is worse than falling back to the
working directory.

### Check it without creating it

```sh
sbx validate                    # ./sandbox.json
sbx validate path/to/spec.json
```

Reads the file, resolves ports and ordering, and creates nothing - so a pre-commit hook or a
lint job can check a committed spec without a docker daemon. It runs the same loader `create`
does, so lint never drifts from create.

It also names anything it merely dislikes, like a service with no `health`.

### `init` runs once, not on every wake

Schemas, users, seed data. A woken container already has whatever this created, so re-running
it would be at best wasted and at worst destructive.

### `cpu` and `memory` are the ceiling a laptop needs

```json
{ "image": "postgres:16-alpine", "ports": [5432], "cpu": "0.5", "memory": "512m" }
```

Unset means unlimited, fine for one sandbox and not for twenty: a machine running a sandbox per
branch otherwise has no ceiling, and it is the machine that fails, not the sandbox. Docker gets
`--cpus`/`--memory`; a cluster gets `resources.limits`. Requests are left alone in the cluster
case - those change scheduling, the operator's business.

### `egress: "deny"` blocks the way out, not the way in

```json
{ "image": "node:22", "ports": [3000], "egress": "deny" }
```

Docker gets a per-sandbox bridge with IP masquerade disabled: no NAT off the host, so nothing
routed leaves - and docker still publishes ports into it, so waking is untouched. Verified both
directions: the service could not fetch `example.com`, and it still woke on a connection and
answered 200.

The obvious alternatives were tried and do not work: `--internal` and `--network none` both
block egress **and** stop docker publishing the port, producing a sandbox that can never be
woken. → [DECISIONS.md](DECISIONS.md)

For a **domain allow-list** rather than all-or-nothing, use `egress_allow` (below) - the
filtering proxy this once said would be needed, now built. DNS still resolves under plain `deny` -
docker's resolver sits on the bridge and needs no route out.

The kubernetes provider **refuses** a service that declares it rather than starting one with
egress open: the cluster answer is a NetworkPolicy, only some CNIs enforce them, and a
security control that silently did nothing is worse than one that says no.

### `egress_allow` is a domain allow-list

```json
{ "image": "python:3.12", "ports": [8000], "egress_allow": ["api.openai.com", "pypi.org"] }
```

The service reaches only the listed hosts and nothing else - an agent box that may call an LLM API
and its package registry, and no other address. Each entry matches the host and its subdomains, so
`openai.com` permits `api.openai.com`. It is the no-NAT bridge of `egress: "deny"` plus a filtering
proxy sbx runs on the bridge gateway: `HTTP_PROXY`/`HTTPS_PROXY` point every client at it, and a
client that ignores the proxy and dials out directly has no route at all, so the list is enforced,
not advisory. It is not combined with `egress: "deny"`, which would deny the allowed hosts too.

**Where the filter runs.** Two arrangements, chosen for you. Where `sbx serve` can bind the
sandbox's bridge gateway - a native Linux docker - the filter is a listener inside the daemon.
Where it cannot, which is every VM-backed docker (colima, Docker Desktop, rootless, and so every
Mac), the same filter runs as a small container on that bridge instead. It is built once per
machine from sbx's own source, so the two are the same code and cannot drift apart.

That container is dual-homed: on the sandbox's no-NAT bridge, where the workload can reach it, and
on an ordinary bridge, where it can reach the internet. The workload still has no route out of its
own, so a client that ignores `HTTP_PROXY` and dials a host directly gets nowhere - measured, on a
Mac: an allowed host returns its page, a host off the list gets `403 Forbidden` from the filter,
and the same fetch with the proxy variables unset times out with no route at all.

`egress_allow` was refused outright on those platforms before. It is not any more.

The traffic through that proxy also counts as activity. A box running an agent takes no inbound
connection - it reads files, compiles, and calls an API - so on the bytes sbx measures it looks
idle from the moment it starts working, and the only setting that kept it alive was `idle: "never"`,
which holds its memory for as long as the sandbox exists. An allow-listed box's API calls leave
through code sbx already owns, so they are counted: it stays awake while it is calling out, and
sleeps on the ordinary timer once it stops. Stamped on bytes rather than on connections, so a
streaming response keeps it awake for as long as tokens are arriving, and throttled to one stamp a
second, which is far finer than an idle window measured in minutes.

**What the stamp reaches.** A sandbox has one bridge and so one filter, and there is nothing in an
HTTP CONNECT that says which container opened it - so the stamp marks every service on that bridge
**that declared an allow-list of its own**. A service with no `egress_allow` is not on it and
sleeps on its ordinary timer. Measured: an allow-listed box calling out every five seconds stayed
awake through twelve consecutive 30-second idle windows, while a plain service beside it in the
same sandbox slept on schedule. Two allow-listed services in one sandbox do keep each other awake;
if that matters, put them in different sandboxes.

### `idle` keeps a box awake while it works

```json
{ "image": "ubuntu:24.04", "ports": [7777], "idle": "never" }
```

sbx sleeps a service after the idle window with no traffic through its port. An agent doing work
*inside* the box - a long command, a compute loop, waiting on an API - sends nothing through sbx,
so the default timer would sleep the container and end the work. `"never"` (or `"0"`) keeps it
awake until you sleep or remove it; a duration like `"30m"` sets a longer window. The box still
wakes on a connection like everything else - this only changes when it goes back to sleep.

A box with `egress_allow` needs this less. Its calls out are counted as activity, so it stays awake
while it is working and sleeps when it stops - which is what `"never"` cannot do.

The stamp reaches every service on the sandbox's bridge that declared an allow-list of its own; a
service without one sleeps on its ordinary timer regardless.

### `optional` still reserves its ports

A branch that never queries the analytics store shouldn't pay for one. But the ports stay
reserved, so adding it later doesn't renumber everything else.
→ [DECISIONS.md](DECISIONS.md#optional-services-still-reserve-their-ports)

---

## Or skip the file entirely

```sh
sbx templates                             # analytics browser nginx postgres web-stack
sbx create my-site --template nginx
```

The templates are the [`examples/`](../examples/), embedded in the binary - so an agent asked
to spin up a Postgres can do it in one line, with nothing on disk.

---

## Coming from docker-compose

Most teams already declare their backing services in a `docker-compose.yml`, and a compose
service is close to isomorphic to an sbx one. The mapping, field for field:

| docker-compose | sandbox.json | note |
|---|---|---|
| `image` | `image` | the same, and pin it |
| `build.context` / `build.dockerfile` | `build.context` / `build.dockerfile` | sbx tags by a hash of the context |
| `ports: ["5432:5432"]` | `ports: [5432]` | **container side only** - the host side is assigned from the sandbox's slot |
| `environment` | `env` | `${VAR}` works in both |
| `command` | `args` | appended to the image's entrypoint |
| `volumes: ["pgdata:/var/lib/postgresql/data"]` | `volume: "/var/lib/postgresql/data"` | one per service, named after the sandbox |
| `volumes: ["./my.conf:/etc/my.conf:ro"]` | `files: {"./my.conf": "/etc/my.conf"}` | read-only, relative to the spec |
| `healthcheck.test` | `health` | a shell command, run inside the container |
| `depends_on` | `depends_on` | ordering only, as in compose without a condition |
| `deploy.resources.limits` | `cpu`, `memory` | |
| `profiles` | `optional` | created with `--optional` |
| - | `exports` | the piece compose has no equivalent of, and the reason adoption is cheap |

**The one that surprises people is `ports`.** Compose lets you choose the host port; sbx does
not - you declare the container port and read the assigned one back through `exports`.

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
      "health": "pg_isready -U app"
    },
    "cache": { "image": "redis:7-alpine", "ports": [6379], "health": "redis-cli ping" }
  },
  "exports": { "DATABASE_PORT": "db:5432", "REDIS_PORT": "cache:6379" }
}
```

Anything reading `DATABASE_PORT` keeps working; `docker compose up` had it on 5432 and sbx
has it wherever this sandbox's slot puts it.

**What does not carry over**: compose's `networks` (each sandbox gets its own), `restart`
(the daemon owns lifecycle - there is no start and no stop), and `depends_on` *conditions*
(sbx waits for health before creating a dependent, which is the `service_healthy` behaviour;
there is no other condition to choose).

`sbx validate` checks the result without creating anything.
