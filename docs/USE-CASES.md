# Use cases

Four shapes this fits, and the ones it does not.

---

## 1 · Several branches at once

**The problem.** Every branch shares one database, so a migration on one is a migration on
all. The alternative — a stack per branch — costs full memory for every branch you ever
opened.

```
   before                          after
   ──────────────────────          ──────────────────────
   branch A ─┐                     branch A ─▶ own db  ● awake
   branch B ─┼─▶ one db            branch B ─▶ own db  ○ asleep  0 B
   branch C ─┘   shared state      branch C ─▶ own db  ○ asleep  0 B
```

Only what you are looking at is resident. **Three attached sandboxes ≈ 2.2 GB against 5.7 GB
for three copies of an untuned stack.**

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

**Why this matters more than it sounds:** the hosted sandbox platforms expose
create/pause/resume through an SDK, so *something has to call resume*. An agent's clients
here are `psql`, a connection pool, a test runner — none of which can call an SDK. They open
a socket, and the socket is the wake signal.

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
