# Use cases

Six shapes this fits, and the ones it does not — the problem each one solves.
The commands per situation are in the [README](../README.md#use-it).

---

## 1 · Several branches at once

**The problem.** Every branch shares one database, so a migration on one is a migration on
all. The alternative — a stack per branch — costs full memory for every branch you ever
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

Only what you are looking at is resident. **Three attached sandboxes ≈ 2.2 GB against 5.7 GB
for three copies of an untuned stack**, and the three that nobody has queried cost nothing at
all rather than a third each.

---

## 2 · An agent that needs somewhere to work

An agent mid-task wants a Postgres to try a migration against, or a second Redis to
reproduce a cache bug.

```sh
sbx add my-task pg --image postgres:16-alpine --port 5432 \
  --health "pg_isready -U postgres" --env POSTGRES_PASSWORD=pw
```

It gets a port from the sandbox's block, sleeps when idle like everything else, and is
destroyed with the sandbox — instead of a stray container that outlives the task and belongs
to nobody.

**Why this matters more than it sounds:** an agent's clients here are `psql`, a connection
pool, a test runner — none of which can call an SDK to wake anything. They open a socket, and
the socket is the wake signal. → [COMPARISON.md](COMPARISON.md#the-axis-that-actually-separates-them)

---

## 3 · A build that needs the stack up

```sh
sbx create "$BRANCH" && sbx ready "$BRANCH"
eval "$(sbx env "$BRANCH")"
./run-tests.sh
```

No daemon needed for a one-shot run: `ready` starts what it needs and blocks until it is
serving. On a persistent runner, leaving the sandbox behind is the interesting case — the
next job on that branch reuses warm, migrated state and pays one wake instead of a create.

A harness can gate on it without ever starting anything:

```json
"env": { "ready": "sbx ready $SANDBOX", "readyTimeout": 120 }
```

Asking *is* starting, which is why there is no `up`.

---

## 4 · A link for somebody else

```sh
sbx url my-branch web
#   https://….trycloudflare.com
```

The tunnel points at the **wake port**, so the sandbox behind a shared link is asleep until
somebody opens it. A reviewer clicks, waits about a second, sees the app.

---

## 5 · A browser, on the same terms

Nothing about this is database-shaped. A headless Chrome is a container that speaks TCP, so
it sleeps and wakes like everything else:

```sh
sbx create my-branch --spec examples/browser/sandbox.json
curl "http://$CDP_HOST:$CDP_PORT/json/version"
# {"Browser": "HeadlessChrome/124.0.6367.78", ...}
```

Asleep at 0 B → woken by that request in **624 ms** → then driven over CDP by Playwright or
Puppeteer, which never learn they started something. A scrape job that runs twice a day stops
being a browser you pay to keep alive.

⚠️ Chrome images often ship without `wget` or `curl`, which makes the health command the
thing that breaks. → [SPEC.md](SPEC.md#health-is-close-to-required)

---

## 6 · One seeded database, many agents

The expensive part of a per-task sandbox is rarely the container — it is getting the data
into it. A schema, a migration, a fixture set: doing that once per agent is what makes
"a sandbox each" sound extravagant.

```sh
sbx exec main postgres psql -U app -d app -f schema.sql
sbx snapshot main golden

sbx fork golden agent-1
sbx fork golden agent-2
```

Each fork has its own copy and its own ports; a write in one is invisible to the others and
to the original. The migration runs once.

⚠️ **Filesystem state, not memory.** A fork starts its services cold against warm data,
exactly as a wake does. It is not a paused process resumed — E2B and zeropod do that, in
tens of milliseconds, using Firecracker or CRIU. `sbx doctor` reports whether this machine
has either.

It snapshots the **volume**, which is worth knowing if you are reasoning about what is
captured: `docker commit` does not include mounted volumes, and in sbx every byte worth
saving is in one. → [DECISIONS.md](DECISIONS.md)

---

## Where to use something else

| If you need | Use |
|---|---|
| An agent running genuinely untrusted code | E2B, Vercel Sandbox, Modal — Firecracker microVMs |
| An agent's REPL resumed mid-thought | E2B — it snapshots RAM; this restores disk only |
| A URL per pull request, for reviewers | Uffizzi, Okteto, Northflank |
| Only ephemeral test fixtures | Testcontainers — ephemeral is right there |
| HTTP-only, already on Knative | Knative — mature, and this is not |

→ [COMPARISON.md](COMPARISON.md) for the full table, sourced to vendor documentation.

**Honest limits.** A container shares the host kernel; `--isolation gvisor|kata` is
declarable and refused when the runtime is absent, but operating a hardened cluster is not
something this does for you. And nobody outside its author has run it in production.
