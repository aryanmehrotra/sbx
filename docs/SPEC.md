# The spec

`sandbox.json` — one file, committed to your repo, describing what a branch needs to run.

A spec never says **when** to start or stop anything. It says what exists, how to tell when it
is serving, and how to reach it. Lifecycle belongs to `sbx serve`, which watches the ports. If
a spec could start something, the spec would eventually be the thing that left it running.

---

## A complete one

```json
{
  "version": 1,
  "services": {
    "postgres": {
      "image": "postgres:16-alpine",
      "ports": [5432],
      "health": "pg_isready -U app -d app",
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
      "image": "clickhouse/clickhouse-server:24.3",
      "ports": [8123],
      "health": "wget -qO- localhost:8123/ping",
      "files": { "low-mem.xml": "/etc/clickhouse-server/config.d/low-mem.xml" },
      "optional": true
    }
  },
  "exports": { "DATABASE_PORT": "postgres:5432", "REDIS_PORT": "redis:6379" }
}
```

---

## Top level

| field | required | does |
|---|---|---|
| `version` | ● | `1`. Lets a future format change be detected rather than silently misread |
| `services` | ● | The declared set, keyed by name — a map, so a name is unambiguous when merging |
| `exports` | | Maps port assignments onto the variables your scripts already read |

**`exports` is how adoption stays cheap.** `{"DB_PORT": "mysql:3306"}` becomes
`DB_PORT=<public port of mysql 3306>`. Without it, adopting sbx would mean editing every
script that already knows a port.

---

## Per service

| field | required | does |
|---|---|---|
| `image` | ● | Any container image |
| `ports` | ● | Container-side ports. The public and backing ports are **assigned from the sandbox's slot**, never chosen here |
| `health` | | A command run **inside** the container — how sbx knows it is serving |
| `env` | | Environment variables |
| `args` | | Command arguments, appended to the image's entrypoint |
| `volume` | | One container path to persist. What makes sleeping safe |
| `files` | | Read-only host files, mounted; paths are relative to the spec |
| `init` | | Commands run **once**, after the service first reports healthy |
| `optional` | | Not created unless `--optional` — but still reserves its ports |

### Why ports aren't yours to choose

Two repos that both picked 5432 collide the moment somebody opens both. Each sandbox gets a
slot, and ports are assigned from it. `exports` is the seam that keeps this invisible to your
tooling.

### `health` is close to required

Without it the daemon can only dial the published port — and Docker answers that before the
server inside does. The first query after a wake then lands on a socket that is about to
close. → [DECISIONS.md](DECISIONS.md#a-published-port-is-not-readiness)

⚠️ **The health command must exist in the image.** A Chrome image with no `wget` cannot be
health-checked with `wget`, and the failure looks like a service that never starts. Check
first:

```sh
docker run --rm --entrypoint sh <image> -c 'command -v wget curl'
```

### `init` runs once, not on every wake

Schemas, users, seed data. A woken container already has whatever this created, so re-running
it would be at best wasted and at worst destructive.

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

The templates are the [`examples/`](../examples/), embedded in the binary — so an agent asked
to spin up a Postgres can do it in one line, with nothing on disk.
