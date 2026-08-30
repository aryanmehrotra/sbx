# Use cases

> **Short version:** thirteen shapes this fits - branches, agents, CI, a shared link, a browser,
> seed-and-fan-out, a cluster, a deployed sandbox, a box for your own commands, a test fixture,
> and a parked-and-resumed process. The commands are in the
> [README](../README.md#install); this is the *why* and the numbers.

---

## 1 · Several branches at once

**The problem.** Every branch shares one database - a migration on one is a migration on all.
A stack per branch instead costs full memory for every branch you ever opened.

```
   TODAY                              WITH sbx
   ─────────────────────────          ─────────────────────────────
   branch A ─┐                        branch A ─▶ own db   ● awake
   branch B ─┼─▶ ONE database         branch B ─▶ own db   ○ 0 B
   branch C ─┘   shared state         branch C ─▶ own db   ○ 0 B

   a migration on one                 nothing shared, and you only
   is a migration on all              pay for what you're looking at
```

Only what you are looking at is resident. [BENCHMARKS.md](BENCHMARKS.md) has the figures: a
sleeping sandbox is **0 B of memory**; the tuning table shows an attached one's per-service
cost (`mysql:8.0` 411 MB stock, 110 MB tuned).

---

## 2 · An agent that needs somewhere to work

An agent mid-task wants a Postgres to try a migration against, or a second Redis to
reproduce a cache bug.

```sh
sbx create my-task --template postgres          # the sandbox it adds to
sbx add my-task pg --image postgres:16-alpine --port 5432 \
  --health "pg_isready -U postgres" --env POSTGRES_PASSWORD=pw
```

It gets a port from the sandbox's block, sleeps when idle, and dies with the sandbox - not a
stray container that outlives the task and belongs to nobody.

**Why this matters:** an agent's clients here are `psql`, a connection pool, a test runner -
none can call an SDK to wake anything. They open a socket, and the socket is the wake signal.
→ [COMPARISON.md](COMPARISON.md#the-axis-that-actually-separates-them)

---

## 3 · A build that needs the stack up

```sh
sbx create "$BRANCH" && sbx ready "$BRANCH"
eval "$(sbx env "$BRANCH")"
./run-tests.sh
```

`sbx serve` must be running: `env` exports the public ports, the daemon answers on them, and
`ready` starts what it needs and blocks until it is serving. On a persistent runner, leaving
the sandbox behind lets the next job on that branch reuse warm, migrated state - one wake
instead of a create.

A harness can gate on it without starting anything, in *its own* config - not sandbox.json,
which would reject these fields:

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
Redis. Chrome is a much heavier thing to start, and the first touch pays for warming caches -
this doc carried an unsourced "624 ms" until somebody ran it. The wake is the browser's own
startup, not sbx's: the same Chrome by hand costs the same.

Chrome images often ship without `wget` or `curl`, which makes the health command the thing
that breaks. → [SPEC.md](SPEC.md#health-is-close-to-required)

---

## 6 · One seeded database, many agents

The expensive part of a per-task sandbox is rarely the container - it is getting the data in:
a schema, a migration, a fixture set, redone once per agent, is what makes "a sandbox each"
sound extravagant.

The spec can seed it - fewer commands, and reproducible. `files` mounts your migration in and
`init` runs it once, after the service first reports healthy:

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

Health is `select 1`, not `pg_isready`: `pg_isready` answers yes while postgres is still
bootstrapping, before `POSTGRES_DB` exists, so `init` would run against a database that is not
there yet. → [SPEC.md](SPEC.md)

The seed lives in the committed file, not somebody's shell history, so the next person forking
that snapshot sees how the golden state was made. If the data is too large to commit, or the
sandbox already exists, do it by hand:

```sh
sbx cp   main postgres ./schema.sql :/tmp/schema.sql
sbx exec main postgres psql -U app -d app -f /tmp/schema.sql
```

Each fork has its own copy and its own ports; a write in one is invisible to the others and to
the original.

**Snapshot does not pause the service.** It copies a crash-consistent filesystem - what a
database recovers from after a power cut. If something is actively writing at snapshot time,
the very last write can be lost;
[TROUBLESHOOTING.md](TROUBLESHOOTING.md#a-fork-is-missing-the-write-i-just-made) has the
quiesce recipe. Seed-then-snapshot, the shape above, is unaffected.

**Filesystem state, not memory.** A fork starts its services cold against warm data, exactly
as a wake does. It is not a paused process resumed - E2B and zeropod do that, in about a
second for E2B, and for zeropod's CRIU restore "tens to a few hundred milliseconds" in their
words - measured here at 272 ms. `sbx doctor` reports whether this machine has either.

It snapshots the **volume**: `docker commit` does not include mounted volumes, and in sbx
every byte worth saving is in one. → [DECISIONS.md](DECISIONS.md)

---

## 7 · The same spec, in a cluster

Everything above is a laptop. The spec does not change:

```sh
sbx create my-branch --provider kubernetes --namespace sbx
sbx env    my-branch --provider kubernetes
```

Services become Deployments and Services in that namespace; `exports` resolves to
cluster-internal addresses instead of `127.0.0.1`, so your tooling reads the same variable
pointing somewhere else. `deploy/` has the activator that plays the daemon's part inside the
cluster.

Two things behave differently on purpose, and both say so rather than half-working:

- **`build:` is refused.** Building in a cluster means pushing to a registry the nodes can
  pull from - credentials, an address, a retention policy - none of which sbx can assume
  without becoming an opinionated CI system. Build it yourself and name it with `image`.
- **`egress: "deny"` is refused.** The cluster equivalent is a NetworkPolicy, enforced only by
  some CNIs. Applying one and reporting success would leave a service wide open on a cluster
  whose CNI ignores it, while the spec said deny.

`sbx url` also refuses and points at an Ingress, because a tunnel to a pod is not a thing sbx
should be inventing on your cluster's behalf.

---

## 8 · A sandbox that is not on your laptop

The laptop that cannot run the stack - four services on eight gigabytes, or an agent you would
rather keep off the machine. Put the service on whatever platform you already deploy to, and
keep talking to it as though it were local:

```sh
sbx pack --spec sandbox.json          # one build context per service, under sbx-pack/

# deploy sbx-pack/db/ and sbx-pack/cache/, each with SBX_CONNECT_TOKEN in its environment

SBX_CONNECT_TOKEN_DB=... SBX_CONNECT_TOKEN_CACHE=... \
  sbx connect db=https://db.example.dev cache=https://cache.example.dev
#   db     ->  127.0.0.1:5432
#   cache  ->  127.0.0.1:6379
```

`psql -h 127.0.0.1 -p 5432` then connects to a database in someone else's datacentre without
knowing it - the tools stay ordinary.

**A sandbox is a group of services, and this is where the group is put back together.** A
platform that gives one container per service spreads it over several deployments; naming them
merges their ports into one map here. The alternative - one container running everything -
means giving up the image each service's spec named, since two cannot generally be merged:
grafting a musl binary into a glibc base builds cleanly and fails at runtime, a deploy that
looks fine until somebody connects. Each deployment stays its own image; the joining happens
here.

**And you can watch it.** The same URLs and tokens point the dashboard at a deployment rather
than at this machine:

```sh
SBX_CONNECT_TOKEN_DB=... SBX_CONNECT_TOKEN_CACHE=... \
  sbx ui --connect db=https://db.example.dev --connect cache=https://cache.example.dev
```

Several deployments merge into one screen, each service showing its state, cpu, memory and
whatever ceiling it has - a fronted deployment reports its own cpu and memory from the
container's cgroup, so the columns fill even with no runtime behind it. The dashboard's keys
act on the deployment too: wake, sleep, `L` limit, `d` remove and `l` logs, each authorised by
the connect token. And **`f` forwards** the selected service's ports to this machine, so `psql`
or `redis-cli` reaches a database that is not exposed - the same tunnel `sbx connect` opens,
but for one service, held until you quit. Usage is sampled only when the dashboard asks, so an
ordinary `sbx connect` costs the deployment no more than before.

A named deployment reads `SBX_CONNECT_TOKEN_<NAME>` before the shared `SBX_CONNECT_TOKEN`, so
two deployments need not share one secret. If two front the same port - two postgres, a normal
thing to want - `--port-offset replica=1000` moves one and says so in the listing.

A platform-as-a-service gives you one container and one HTTP port, so that is the shape `pack`
writes. The generated image runs the base image's **own** start line - read out of the image,
not guessed - beside `sbx serve --connect-addr --front`, and the tunnel is a WebSocket because
an L7 proxy strips anything more exotic. `connect` binds `127.0.0.1` only, on the same port
numbers the deployment uses, so the `sbx env` block from over there is already correct here.

Four things are worth knowing before you rely on it:

- **One deployment per service.** A property of the platform, not a limitation here:
  `sbx connect` takes as many as you give it. The cost is a deploy per service, and whatever
  your platform charges for it.
- **No volume means no data.** Most platforms replace the container rather than keep its disk -
  right for a branch's test fixtures, wrong for anything you would miss.
- **The token is the whole of the security.** Anyone with the URL and `SBX_CONNECT_TOKEN` has
  the port, from anywhere. → [SECURITY.md](../SECURITY.md)
- **It is a network away.** Every round trip now costs what the internet costs, which a test
  suite making thousands of them will notice.

→ [DECISIONS.md](DECISIONS.md) for why this contradicts an earlier decision, and what changed.

---

## 9 · A box to run your own commands in

Everything above puts a *service* in a sandbox - a database, a cache, a browser - and
`sbx exec` drops you into that service's container, which holds the service and nothing else.
A postgres image has psql and no git; an alpine service image has neither. So "run the build
inside the sandbox" needs a different service: one whose job is to hold your tools and your
source.

That is a spec, not a feature. Declare a service with the toolchain you want and mount the
repository into it:

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
sbx exec -t my-branch dev sh          # a shell, in /work, with your code in it
sbx exec my-branch dev go test ./...  # or just run the thing
```

Four things about that file are load-bearing:

- **`mounts` is your source**, and a relative path resolves against the directory the spec is
  in - so `"."` is the repository and the file stays portable. Writes go both ways: a file the
  container creates is a file in your editor.
- **`args` keeps it alive.** A container whose command finishes exits, and `sleep infinity` is
  the usual way to say "stay up and wait to be asked".
- **`ports` is required even here**, and nothing listens on 7777. Every other service in sbx is
  reached by opening a socket, so the spec asks for one; a box you only `exec` into has no use
  for it. Declare one and ignore it.
- **The image is the toolchain.** `golang:1.26-alpine`, `node:22`, your own CI image - whatever
  the commands you mean to run need. This is the one service where a fat image is the point.

It sleeps like everything else, and `sbx exec` wakes it: exec against a stopped container would
fail with a message about the container rather than the sandbox being asleep, so sbx starts it
first and waits. The cost of leaving one around is the same 0 B.

**On macOS and Windows the mount has to be somewhere the VM shares** - under your home
directory, typically. sbx checks by writing a marker on one side and looking for it on the
other, so a path Docker Desktop or colima is not sharing is refused at create time with the
reason, rather than presenting an empty directory that looks like a missing repository.

---

## 10 · A fixture that lives exactly as long as a test

**The problem.** A test needs a real Postgres, and a `create`/`env`/`rm` script leaks the sandbox
the moment the test panics or the runner is killed - the exact failure Testcontainers exists to
prevent.

```sh
sbx with test-db --template postgres -- go test ./...
```

`sbx with` creates the sandbox, waits until it serves, runs the command with the sandbox's env
exported, and **always removes it afterwards** - on success, on a failing test, or on an
interrupt. The command's own exit status becomes sbx's, so CI gates on it unchanged. `--keep`
leaves it for inspection after a failure. This is the opposite lifecycle from a branch sandbox: a
test fixture that survives the test is a leak, not a saving.

---

## 11 · Parking a process and resuming it where it was

**The problem.** A wake starts a service cold against a warm disk - Postgres replays its WAL, a
REPL loses its variables, a cache comes up empty. Sometimes you want the *process* back, not just
its data.

```sh
sbx checkpoint agent-42 mid-thought    # freeze memory + processes
sbx resume     agent-42 mid-thought    # bring them back exactly as they were
```

`sbx checkpoint` CRIU-dumps every running service's memory and freezes it; `sbx resume` restores
it. This needs a Linux host and is proven on a **podman** runtime, whose CRIU restore is reliable
where docker's is broken; it is refused with a reason on macOS. Filesystem-only save-and-fan-out
is [use case 6](#6--one-seeded-database-many-agents); this is the memory one.

---

## Isolation

A container shares the host kernel by default; `--isolation gvisor|kata` gives a stronger
boundary, refused with a reason when the runtime is absent.

---

## 12 · An editor in the sandbox, without a second machine to pay for

**The problem.** The work is in the sandbox — the code, the database it talks to, the tools that
match CI — and the editor is on a laptop that has none of it. The usual answer is a hosted dev
environment: a second machine, running whether or not anybody is typing, billed for the hours
nobody was.

`sbx ssh` reaches the sandbox you already have, from the editor you already use.

```json
{
  "version": 1,
  "services": {
    "dev": {
      "image": "lscr.io/linuxserver/openssh-server:latest",
      "ports": [2222],
      "mounts": { ".": "/work" },
      "env": { "USER_NAME": "dev", "PASSWORD_ACCESS": "true", "USER_PASSWORD": "..." }
    },
    "postgres": { "image": "postgres:16-alpine", "ports": [5432], "health": "pg_isready -U postgres" }
  }
}
```

```sh
SBX_FEATURES=ssh sbx ssh feature-x --user dev
  ssh -p 20060 dev@127.0.0.1
  code --remote ssh-remote+dev@127.0.0.1:20060 /work
```

**Why this works at all is the interesting part.** An SSH connection is an ordinary inbound TCP
dial, which is the one thing sbx already wakes on — so the editor needs no plugin, the daemon
needs no editor-specific path, and JetBrains Gateway, `scp` and `rsync` work for the same reason.
Measured: a sandbox asleep at 0 B, `ssh-keyscan` against it, **awake and through the SSH handshake
in 0.255 s.**

The two other VS Code remote modes cannot do this, and it is worth knowing why before reaching
for one. *Attach to Container* talks to the docker socket and never opens a connection to the
container's network — nothing is dialled, so nothing wakes. *Remote-Tunnels* needs a `code tunnel`
process already running inside, which means the sandbox is already up.

**What it does not do.** It does not sleep while the editor is attached. Nothing does — VS Code's
protocol pings every five seconds unconditionally, so an attached editor is never idle, and CRIU
cannot restore a TCP socket across a network hop, which is why zeropod dropped the attempt. Close
the window and the sandbox sleeps on its own idle timer, with the database beside it.

**It is behind a gate.** `sbx ssh` is preview: `SBX_FEATURES=ssh`. The command is small and the
contract may still move — `sbx features` lists what a build gates and what is on.

---

## 13 · A repo that already has a devcontainer

**The problem.** The repository has `.devcontainer/devcontainer.json`. It already says what image
to run, what ports to publish, what to mount and what to do once it is up. Being asked to write
all of that again in a second file, in a different shape, is the reason most people stop reading
at the install line.

```sh
SBX_FEATURES=devcontainer sbx init --from-devcontainer . > sandbox.json
```

It reads the file where the spec allows it to live — `.devcontainer/devcontainer.json`,
`.devcontainer.json`, or the bare name — comments and trailing commas included, because
devcontainer.json is JSON-with-comments and VS Code's own templates ship with them.

What comes across: the image or the build, `forwardPorts` **and** the legacy `appPort` (a repo
untouched for three years is exactly the one being imported), `containerEnv` and `remoteEnv`, the
workspace folder as a mount, and the three run-once lifecycle hooks in the order the spec fixes
them — `onCreate`, then `updateContent`, then `postCreate`. Getting that order wrong runs a build
before the thing it builds has been fetched.

**What does not come across is printed, on stderr, so the redirect above still writes a clean
file.** Features are not installed. `postStartCommand` and `postAttachCommand` have no equivalent
— `init` runs once. `remoteUser` belongs to `sbx ssh --user`, not the spec. A `dockerComposeFile`
is refused outright rather than guessed at: compose describes several services and how they
connect, which is what `sandbox.json` is for, so importing one service out of it would mean
picking which one somebody meant.

**Then add the rest.** A devcontainer describes one container; a sandbox describes what a branch
needs. The import gives you the first service — the database and the cache go beside it, and now
they sleep when nobody is using them.

**It is behind a gate.** `SBX_FEATURES=devcontainer`. The translation is lossy by nature and the
list of what it drops will move as it learns more; `sbx features` shows what a build gates.

---

## 14 · An agent box that only ever calls out

The box in case 2 has a client: `psql`, a test runner, a pool. This one has none. An agent works
*inside* it — reads files, edits them, compiles, calls a model API — and nothing ever dials it.

sbx measures idleness on bytes through a service's port, so a box like that looks idle from the
moment it starts working. The window closes mid-task and the container stops. The only setting
that avoided it was `idle: "never"`, which is not a fix — it holds the box's memory for as long
as the sandbox exists, which is the cost sbx was built to avoid.

An allow-list changes that, because it puts sbx back in the path:

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

The agent reaches those three hosts and nothing else — and **every call it makes counts as
activity**. A box that is working stays awake. A box whose agent finished sleeps ten minutes
later, on the ordinary timer, and drops to 0 B like everything else.

**Why this matters:** the outbound call is the one thing an agent does that sbx can see. Reading
a file is invisible, a compile is invisible, but reaching a model API goes through a proxy sbx
already runs to enforce the allow-list — so counting it costs nothing new. It is stamped on bytes
rather than on connections, which is the same rule sbx measures inbound traffic by: a streaming
response that takes four minutes to arrive keeps the box awake for four minutes, where a stamp
per connection would have slept it at minute one.

**What it does not cover:** a box with no allow-list, or one calling out over a protocol that is
not HTTP. There sbx still sees nothing, and `idle: "never"` remains the answer.
→ [SPEC.md](SPEC.md#egress_allow-is-a-domain-allow-list)

---

## 15 · A link you send someone, for a sandbox that is asleep

`sbx url` gives a branch preview a public address, and the sandbox behind it sleeps like
everything else. The first person to open the link is the one who wakes it.

```sh
SBX_FEATURES=waiting-page sbx serve --idle 5m &
sbx url my-branch web
```

sbx holds a cold connection rather than refusing it, which is right for every client with a
library behind it — the request just takes a moment longer and then works. A browser is the
exception: a held connection is a white screen, nothing on it says the machine is busy, and the
reasonable conclusion is that the link is broken. The reflex is to reload, which opens a second
connection to a service that is already starting.

With the gate on, a wait longer than a second gets a page that names what is starting and reloads
itself. Measured against a sandbox with a four-second start: **503 with the page at 1.00s, then
200 in 1.5ms on the reload.**

**What it does not do:** it never delays the wake, it never speaks for a wake that is quick, and
it only answers something that is actually an HTTP request — a postgres startup packet, a redis
`PING`, a TLS handshake are carried through untouched, with whatever was read while deciding
replayed to the service first.

**Why it is behind a gate:** the default is still to hold, because holding is what makes sbx
invisible to everything that is not a browser. This is a deliberate exception to that, so it is
one you turn on.
→ [SPEC.md](SPEC.md#idle-keeps-a-box-awake-while-it-works)
