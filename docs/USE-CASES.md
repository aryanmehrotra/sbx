# Use cases

> **Short version:** seven shapes this fits - branches, agents, CI, a shared link, a browser,
> seed-and-fan-out, a cluster - and the ones it does not. The commands are in the
> [README](../README.md#how-to-use); this is the *why* and the numbers.

---

## 1 · Several branches at once

**The problem.** Every branch shares one database, so a migration on one is a migration on
all. The alternative - a stack per branch - costs full memory for every branch you ever
opened.

```
   TODAY                              WITH sbx
   ─────────────────────────          ─────────────────────────────
   branch A ─┐                        branch A ─▶ own db   ● awake
   branch B ─┼─▶ ONE database         branch B ─▶ own db   ○ 0 B
   branch C ─┘   shared state         branch C ─▶ own db   ○ 0 B

   a migration on one                 nothing shared, and you only
   is a migration on all              pay for what you're looking at
```

Only what you are looking at is resident. The measured figures are in
[BENCHMARKS.md](BENCHMARKS.md): a sleeping sandbox is **0 B of memory**, and the tuning table
there shows what an attached one costs per service (`mysql:8.0` 411 MB stock, 110 MB tuned).
Three attached against three sleeping is the difference between paying for what you are
looking at and paying for every branch you ever opened.

---

## 2 · An agent that needs somewhere to work

An agent mid-task wants a Postgres to try a migration against, or a second Redis to
reproduce a cache bug.

```sh
sbx create my-task --template postgres          # the sandbox it adds to
sbx add my-task pg --image postgres:16-alpine --port 5432 \
  --health "pg_isready -U postgres" --env POSTGRES_PASSWORD=pw
```

It gets a port from the sandbox's block, sleeps when idle like everything else, and is
destroyed with the sandbox - instead of a stray container that outlives the task and belongs
to nobody.

**Why this matters more than it sounds:** an agent's clients here are `psql`, a connection
pool, a test runner - none of which can call an SDK to wake anything. They open a socket, and
the socket is the wake signal. → [COMPARISON.md](COMPARISON.md#the-axis-that-actually-separates-them)

---

## 3 · A build that needs the stack up

```sh
sbx create "$BRANCH" && sbx ready "$BRANCH"
eval "$(sbx env "$BRANCH")"
./run-tests.sh
```

`sbx serve` has to be running: `env` exports the public ports and the daemon is what answers
on them. `ready` starts what it needs and blocks until it is
serving. On a persistent runner, leaving the sandbox behind is the interesting case - the
next job on that branch reuses warm, migrated state and pays one wake instead of a create.

A harness can gate on it without ever starting anything. In *its own* config - this is not
sandbox.json, which would reject these fields:

```yaml
# .gitlab-ci.yml, a Makefile target, whatever your runner reads
before_script:
  - sbx ready "$SANDBOX" --timeout 120s
```

Asking *is* starting, which is why there is no `up`.

---

## 4 · A link for somebody else

```sh
sbx url my-branch web
#   https://....trycloudflare.com
```

The tunnel points at the **public port**, so the sandbox behind a shared link is asleep until
somebody opens it. A reviewer clicks, waits about a second, sees the app.

---

## 5 · A browser, on the same terms

Nothing about this is database-shaped. A headless Chrome is a container that speaks TCP, so
it sleeps and wakes like everything else:

```sh
sbx serve --idle 5m &                                        # once per machine
sbx create my-branch --spec examples/browser/sandbox.json
eval "$(sbx env my-branch)"
curl "http://$CDP_HOST:$CDP_PORT/json/version"
# {"Browser": "HeadlessChrome/124.0.6367.78", ...}
```

Asleep at 0 B → woken by that request → then driven over CDP by Playwright or Puppeteer,
which never learn they started something. A scrape job that runs twice a day stops being a
browser you pay to keep alive.

**Measured: about 4.4 s cold, about 0.75 s warm** (n=5, macOS arm64), against 191 ms for
Redis. Chrome is simply a much heavier thing to start, and the first touch pays for warming
caches - this doc carried an unsourced "624 ms" until somebody ran it. The wake is the
browser's own startup, not sbx's: the same Chrome started by hand costs the same.

Chrome images often ship without `wget` or `curl`, which makes the health command the
thing that breaks. → [SPEC.md](SPEC.md#health-is-close-to-required)

---

## 6 · One seeded database, many agents

The expensive part of a per-task sandbox is rarely the container - it is getting the data
into it. A schema, a migration, a fixture set: doing that once per agent is what makes
"a sandbox each" sound extravagant.

The spec can do the seeding, which is both fewer commands and the version that is
reproducible - `files` mounts your migration in and `init` runs it once, after the service
first reports healthy:

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

sbx fork golden agent-1        # the spec is remembered
sbx fork golden agent-2
```

The health command is `psql -U app -d app -c 'select 1'`, not `pg_isready`. `pg_isready`
answers yes while postgres is still bootstrapping, before `POSTGRES_DB` exists - so `init`
runs against a database that is not there yet. → [SPEC.md](SPEC.md)

The seed lives in the committed file rather than in somebody's shell history, so the next
person forking from that snapshot can see how the golden state was made. If the data is too
large to commit, or the sandbox already exists, do it by hand instead:

```sh
sbx cp   main postgres ./schema.sql :/tmp/schema.sql
sbx exec main postgres psql -U app -d app -f /tmp/schema.sql
```

Each fork has its own copy and its own ports; a write in one is invisible to the others and
to the original. The migration runs once.

**Snapshot does not pause the service.** It copies a crash-consistent filesystem - what a
database recovers from after a power cut. If something is actively writing at snapshot time,
the very last write can be lost;
[TROUBLESHOOTING.md](TROUBLESHOOTING.md#a-fork-is-missing-the-write-i-just-made) has the
quiesce recipe. Seed-then-snapshot, the shape above, is unaffected.

**Filesystem state, not memory.** A fork starts its services cold against warm data,
exactly as a wake does. It is not a paused process resumed - E2B and zeropod do that, in
about a second for E2B, and for zeropod's CRIU restore "tens to a few hundred milliseconds"
in their words - measured here at 272 ms. `sbx doctor` reports whether this machine
has either.

It snapshots the **volume**, which is worth knowing if you are reasoning about what is
captured: `docker commit` does not include mounted volumes, and in sbx every byte worth
saving is in one. → [DECISIONS.md](DECISIONS.md)

---

## 7 · The same spec, in a cluster

Everything above is a laptop. The spec does not change:

```sh
sbx create my-branch --provider kubernetes --namespace sbx
sbx env    my-branch --provider kubernetes
```

Services become Deployments and Services in that namespace; `exports` resolves to
cluster-internal addresses instead of `127.0.0.1`, so what your tooling reads is the same
variable pointing somewhere else. `deploy/` has the activator that plays the daemon's part
inside the cluster.

Two things behave differently on purpose, and both say so rather than half-working:

- **`build:` is refused.** Building in a cluster means pushing to a registry the nodes can
  pull from - credentials, an address, a retention policy - none of which sbx can assume
  without becoming an opinionated CI system. Build it yourself and name it with `image`.
- **`egress: "deny"` is refused.** The cluster equivalent is a NetworkPolicy, and a
  NetworkPolicy is only enforced by some CNIs. Applying one and reporting success would leave
  a service wide open on a cluster whose CNI ignores it, while the spec said deny.

`sbx url` also refuses and points at an Ingress, because a tunnel to a pod is not a thing sbx
should be inventing on your cluster's behalf.

---

## 8 · A sandbox that is not on your laptop

The laptop that cannot run the stack - four services on eight gigabytes, or an agent you would
rather keep off the machine entirely. Put the service on whatever platform you already deploy
to, and keep talking to it as though it were local:

```sh
sbx pack --spec sandbox.json          # one build context per service, under sbx-pack/

# deploy sbx-pack/db/ to your platform, with SBX_CONNECT_TOKEN set in its environment

SBX_CONNECT_TOKEN=... sbx connect https://db.example.dev
#   db  ->  127.0.0.1:5432
```

`psql -h 127.0.0.1 -p 5432` then connects to a database in someone else's datacentre without
knowing any of that happened, which is the whole point: the tools stay ordinary.

What makes it work is that a platform-as-a-service gives you one container and one HTTP port,
so that is the shape `pack` writes. The generated image runs the base image's **own** start
line - read out of the image rather than guessed - beside `sbx serve --connect-addr --front`,
and the tunnel is a WebSocket because an L7 proxy will strip anything more exotic. `connect`
binds `127.0.0.1` only, on the same port numbers the deployment uses, so the `sbx env` block
from over there is already correct here.

Four things are worth knowing before you rely on it:

- **One deployment per service, today.** `--front` carries several ports at once, so one
  container can serve a whole sandbox - but `pack` writes one image per service, which means
  two services are two deployments and two `sbx connect`.
- **No volume means no data.** Most platforms replace the container rather than keep its disk.
  That is right for a branch's test fixtures and wrong for anything you would miss.
- **The token is the whole of the security.** Anyone with the URL and `SBX_CONNECT_TOKEN` has
  the port, from anywhere. → [SECURITY.md](../SECURITY.md)
- **It is a network away.** Every round trip now costs what the internet costs, which a test
  suite that makes thousands of them will notice.

→ [DECISIONS.md](DECISIONS.md) for why this contradicts an earlier decision, and what changed.

---

## Where to use something else

| If you need | Use |
|---|---|
| An agent running genuinely untrusted code | E2B, Vercel Sandbox, Modal - Firecracker microVMs |
| An agent's REPL resumed mid-thought | E2B - it snapshots RAM; this restores disk only |
| A URL per pull request, for reviewers | Uffizzi, Okteto, Northflank |
| Only ephemeral test fixtures | Testcontainers - ephemeral is right there |
| HTTP-only, already on Knative | Knative - mature, and this is not |

→ [COMPARISON.md](COMPARISON.md) for the full table, sourced to vendor documentation.

**Honest limits.** A container shares the host kernel; `--isolation gvisor|kata` is
declarable and refused when the runtime is absent, but operating a hardened cluster is not
something this does for you. And it has not yet been run in production outside its own test
suite.
