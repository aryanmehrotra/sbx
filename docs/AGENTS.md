# sbx for coding agents

> **Short version:** paste the block below into your project's `AGENTS.md`, `CLAUDE.md`, or
> whatever your assistant reads. The rest is why each line is there, plus the commands an agent
> reaches for.

An agent on a branch keeps needing somewhere to put a database. The usual answers fail later:
`docker run postgres` leaves a container that outlives the task and belongs to nobody; a shared
database turns a migration tried on one branch into one done to all. sbx gives the task its own,
then takes it away.

---

## The block to paste

```markdown
## Sandboxes (sbx)

When you need a service to do your work - a database to run a migration against, a redis, a
browser, a queue - create a sandbox for it. Do not `docker run` it, do not `docker compose up`,
and do not connect to a shared local database.

    sbx doctor --json                     # what this machine can do; run it first if anything is odd
    sbx list --json                       # what already exists
    sbx create <task> --template postgres # one for this task. Templates: sbx templates
    eval "$(sbx env <task>)"              # exports its addresses into your shell
    sbx ready <task>                      # block until it really answers, for scripts

There is no start and no stop. **Connecting is what wakes a service** - psql, a driver, a test
runner, curl - and idleness puts it back to sleep at 0 B. Never hardcode a port: read it from
`sbx env`, which is the only place the real numbers exist. The variable names come from the
spec's `exports` (the postgres template gives `DATABASE_HOST` and `DATABASE_PORT`), so run
`sbx env <task>` and read them rather than assuming.

    sbx add <task> cache --image redis:7-alpine --port 6379 --health 'redis-cli ping'
    sbx exec <task> postgres psql -U app -d app -c 'select 1'
    sbx logs <task> postgres --tail 50
    sbx rm <task>                         # when the task is done. This deletes its data.

Rules:
- One sandbox per task or branch, named after it. Do not reuse another task's.
- `sbx rm` only the sandbox you created. Somebody else's may be in use.
- Nothing is shared between sandboxes, so a migration in yours affects nothing else.
- If a command fails, run `sbx doctor` before guessing: it says whether docker is even up.
```

Everything below is for the agent that needs more than the common case.

---

## The four things that are not obvious

**1. There is no `up` and no `down`.** A service sleeps until something opens a socket to it —
zero memory, not a stopped container to remember. Connecting is the start; an agent never
"starts" anything. `sbx ready` exists because a script's next line runs immediately, and an open
port is not yet a database that answers.

**2. The ports are assigned, not chosen.** Each sandbox gets a block, so two branches can both
have "a postgres" without knowing about each other. `sbx env` prints the real ones; writing
`localhost:5432` into a config hardcodes another task's database.

**3. One daemon per machine.** `sbx serve` owns every sandbox's ports. If it is not running,
`sbx env` prints addresses nothing answers on — indistinguishable from a broken service.
`sbx doctor` says so in one line.

**4. A sandbox is the unit.** `sbx env`, `sbx rm`, `sbx logs` and `sbx snapshot` all take a
sandbox name, and a sandbox holds several services. In `sbx ui`, `v` shows that shape.

---

## Recipes

**Try a migration without touching anything shared**

```sh
sbx create migrate-users --template postgres
eval "$(sbx env migrate-users)"
psql -h "$DATABASE_HOST" -p "$DATABASE_PORT" -U app -d app -f migrations/007.sql
```

**Seed once, then give every attempt its own copy**

```sh
sbx create golden --template postgres
sbx exec golden postgres psql -U app -d app -f schema.sql
sbx snapshot golden ready

sbx fork ready attempt-1        # a full copy, its own ports
sbx fork ready attempt-2        # a write in one is invisible to the other
```

This changes what is affordable: the cost of a per-task database is the seeding, not the
container — pay it once.

**A service the spec never mentioned, mid-task**

```sh
sbx add my-task cache --image redis:7-alpine --port 6379 --health 'redis-cli ping'
```

**Reproduce a bug against the exact version**

```sh
sbx create repro --spec sandbox.json   # pinned images, committed with the repo
sbx ready repro
```

**Somewhere to run your own commands, with the repository in it**

`sbx exec` drops you into the named service's container, but a postgres image has psql and no
git. To run a build, script or test suite in the sandbox, declare a service that holds the
toolchain and mounts the source:

```json
{ "dev": { "image": "golang:1.26-alpine", "ports": [7777],
           "args": ["sleep", "infinity"], "mounts": { ".": "/work" } } }
```

```sh
sbx exec -t my-task dev sh            # a shell in /work with the code in it
sbx exec my-task dev go test ./...
```

`args` keeps it alive - a container whose command exits is gone. `ports` is required by the
spec but unused here: everything else in sbx is reached over a socket; this service is the
exception. A relative mount resolves against the spec's directory, so `"."` is the repository.
→ [USE-CASES.md](USE-CASES.md#9--a-box-to-run-your-own-commands-in)

**Find out what is going on**

```sh
sbx list --json                        # what exists, awake or not
sbx logs my-task postgres --tail 100   # reading logs does not wake anything
sbx history my-task --json             # every wake, sleep and change, newline-delimited
sbx ui                                 # live, if there is a terminal. v groups by sandbox
```

**Clean up**

```sh
sbx rm my-task                         # the sandbox and its data
sbx gc                                 # what dead sandboxes left. Lists; --force deletes
```

---

## Machine-readable surfaces

An agent should parse these rather than the human tables:

| | |
|---|---|
| `sbx list --json` | every sandbox and service, with `awake`, `addresses`, `ref` |
| `sbx env <sandbox> --shell json` | the addresses as an object, for anything that is not a shell |
| `sbx doctor --json` | capabilities, each absent one saying what its absence costs |
| `sbx history [sandbox] --json` | newline-delimited records: wakes, sleeps, and commands that changed something |

`sbx validate` checks a spec and creates nothing — the cheap way to check a file just written.

---

## What it will refuse, and why

Knowing these saves an agent a turn spent fighting them:

- **`build:` in a spec, on kubernetes.** Building in a cluster needs a registry the nodes can
  pull from, which sbx cannot assume. Name an `image` instead.
- **`egress: "deny"` on kubernetes.** The equivalent is a NetworkPolicy, which only some CNIs
  enforce; applying it and reporting success would leave a service wide open against the spec.
- **`sbx url` on kubernetes.** It points at an Ingress rather than inventing a tunnel to a pod.
- **Clearing a limit on docker.** Docker cannot remove a ceiling from an existing container;
  recreate the sandbox. A cluster can, and is allowed to.
- **`sbx connect` to an `http://` URL not on this machine.** The token would cross the network in
  the clear.

Every refusal names the field or the flag it came from, so the message is usually the fix.

---

## When the machine is not the one running the services

`sbx pack` writes a deployable image per service, and `sbx connect` turns those deployments back
into ordinary local ports - several at once, as one port map:

```sh
sbx connect db=https://db.example.dev cache=https://cache.example.dev
#   db     ->  127.0.0.1:5432
#   cache  ->  127.0.0.1:6379
```

Then `psql -h 127.0.0.1 -p 5432` reaches a database elsewhere without knowing anything happened.
→ [USE-CASES.md](USE-CASES.md#8--a-sandbox-that-is-not-on-your-laptop)

---

## If something is wrong

Run `sbx doctor` first. Most of what an agent hits is one of four things — the daemon not
running, most often by a distance.

→ [TROUBLESHOOTING.md](TROUBLESHOOTING.md) is written as symptom → cause → fix — point an agent
at it directly when it is stuck.
