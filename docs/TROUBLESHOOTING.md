# When something is wrong

Keyed by what you actually see, not by what is actually wrong - those are different, and the
second one is what this page is for.

Run this first. It answers four of the entries below on its own:

```sh
sbx doctor
```

---

## "connection refused" on the port `sbx env` printed

**Almost always: no `sbx serve` is running.** The ports `sbx env` exports are the daemon's,
not docker's - docker publishes a *backing* port and the daemon owns the public one. Without
a daemon, `sbx env` prints an address with nothing behind it.

```sh
sbx doctor | grep 'sbx serve'
sbx serve --idle 5m &          # once per machine, not once per sandbox
```

`sbx create` checks this and says so, and [`deploy/`](../deploy/) has a launchd plist and a
systemd unit for running it supervised so it does not die with your terminal.

**If a daemon *is* running:** it discovers new sandboxes on its `--refresh` interval (15 s by
default), so a sandbox created seconds ago may not be fronted yet. `sbx ready <name>` waits.

---

## "never became ready within 2m0s"

The service started and its health command never passed.

**If the message names the command and says "cannot run in this image"**, the command is not
inside the container - the single most common first-run mistake. A health command runs *in*
the container, so `pg_isready` needs postgres tooling in that image and `curl` needs curl:

```sh
docker run --rm --entrypoint sh <image> -c 'command -v pg_isready curl wget'
```

**Otherwise the workload really is not coming up.** Look at what it said:

```sh
sbx logs <sandbox> <service> --tail 50
```

`logs` is the one command that does not wake anything, so this is safe on a sleeping sandbox.

---

## The service's config file is a directory inside the container

You declared `files: {"./my.conf": "/etc/thing/my.conf"}` and the container behaves as if the
file is empty or missing.

**The container runtime could not reach the host path, so docker created an empty directory
at the destination** rather than failing. A VM-backed docker - Colima, Docker Desktop - only
shares some host paths; `/var/folders` on macOS is usually not one of them, and `$HOME`
usually is.

sbx checks for this after create and says so by name. Move the file under your home directory.

---

## `sbx list` shows nothing, or a sandbox you cannot remove

`sbx list` is rebuilt from labels on the containers themselves. A container whose `sbx.ports`
label will not parse is skipped, so it becomes invisible to `list` and to `rm`.

```sh
docker ps -a --filter label=sbx.sandbox --format '{{.Names}}\t{{.Labels}}'
docker rm -f <name>            # the escape hatch; sbx is not hiding anything from you
```

---

## `sbx serve` says it is already running

One daemon serves the whole machine - it owns every sandbox's public ports, so a second one
would bind nothing. If the pid it names is gone, the record is stale and the next start
clears it. If it is alive, you already have what you were about to start.

---

## The first query after an idle period fails, but the next one works

**Your client's connect timeout is shorter than the wake.** The wake is paid on `connect`,
not on the query - the caller sees a slow first connection, and a client configured to give
up in two seconds will give up.

Typical wakes: ~191 ms for redis, a second or so for postgres, several seconds for a browser
on a cold cache. Raise the connect timeout above that:

| client | knob |
|---|---|
| libpq / psql | `PGCONNECT_TIMEOUT`, or `connect_timeout=` in the URL |
| JDBC | `connectTimeout` |
| Playwright / Puppeteer | the launch/connect timeout, not the navigation one |

A pooled client also has to tolerate a server-initiated close: sleeping a sandbox closes the
connections it is holding.

---

## Wakes are slower than the numbers in BENCHMARKS.md

**A service with no `health` command costs a flat 2 s per wake.** With nothing to ask, the
daemon cannot tell "the port is bound" from "the server is ready" - docker binds the host side
of a published port the instant the container starts - so it waits a fixed moment and goes.
Declaring `health` replaces that with an actual check, and is why every bundled template has
one. → [SPEC.md](SPEC.md)

**A first wake on a cold machine includes the image pull.** `sbx prewarm` moves it somewhere
you can cache.

---

## `sbx create` is slow the more sandboxes exist

It should not be. Creating a sandbox lists the existing ones to pick a free port slot, and
that used to fork a `docker inspect` per container - it is one API call now. If you still see
it, that is a bug worth reporting with the output of `sbx list | wc -l`.

---

## Two `sbx create` at the same moment fail on a port conflict

Slot allocation reads which slots are spoken for and takes the first gap, but the ports are
only really claimed when a container binds them - so two creates racing can be handed the same
gap. sbx narrows this from both sides: a lock under `~/.sbx` serialises the claim on one
machine, and `AllocSlot` binds a candidate slot's backing ports before returning it, which is
the same question docker asks a moment later.

Neither closes it completely, and nothing can from inside one process: two machines driving
one remote `DOCKER_HOST` share no lock. Measured on a laptop, four concurrent creates repeated five times: 5 of 20 succeeded before,
17 of 20 after. **If one fails, retry it** - the retry sees the winner's
containers and takes the next slot.

On a VM-backed docker (Colima, Docker Desktop) the host-side port forward can outlive the
container it belonged to by a few seconds, so a create immediately after an `sbx rm` can
collide with a forward docker already considers gone. Waiting a moment, or retrying, is the
answer there too.

---

## A fork is missing the write I just made

`sbx snapshot` does **not** stop the service first. It takes a crash-consistent copy - the
state the database would recover from after a power cut - because stopping would silently
interrupt whoever is using the sandbox.

Databases are built to survive that, and normally do: an acknowledged Postgres commit is
already in the WAL, and the fork replays it on start. But the copy can catch the WAL mid-write,
and Postgres stops replay at the first torn record - so under heavy load the **last** write
before the snapshot can be missing.

Seen once here, during a run with several suites competing for one docker daemon.

**If the snapshot must be exact**, quiesce it first:

```sh
docker stop sbx-<sandbox>-<service>     # or just stop writing to it
sbx snapshot <sandbox> golden
```

For the ordinary case - seed a database, snapshot it, fan out - nothing is writing to it at
snapshot time and this does not arise.

---

## `sbx connect` cannot reach a deployment the platform calls healthy

The platform's health check and the tunnel are not the same fact. A container can be up, and
serving nothing that answers `sbx connect`. Work down this list - it is ordered by how often
each one was the answer while this was being built.

**"rejected the token".** `SBX_CONNECT_TOKEN` here is not the one the deployment was given.
It is set in two places, and the deployment's copy is the one people forget after a redeploy.

**"the handshake was answered by something that is not this endpoint".** Something is in
front of sbx - a platform login page, a router sending `/` somewhere else, or a URL that
belongs to a different service. Check `curl -sS https://<url>/healthz`: that route needs no
token and answers only if sbx itself is on the other end.

**The deployment is "active" but nothing answers at all.** Almost always the container died
at startup and the platform restarted it quietly. Read its logs. The one that has caught
people is a hand-written Dockerfile installing sbx with `@latest`: releases before this
feature have no `--connect-addr`, so the process exits on an unknown flag. `sbx pack` pins the
version for exactly this reason - if you wrote the image yourself, pin it too.

**"db and replica both want 127.0.0.1:5432".** Two deployments are fronting the same port,
which is what happens when a sandbox has two of the same service. They cannot share one local
port, so move one: `--port-offset replica=1000`. The listing then says which one was shifted,
because its own `sbx env` values no longer apply on this machine.

**"cannot open 127.0.0.1:<port>".** This machine's own `sbx serve` already owns that port,
because the deployment hands out the same numbers it would locally. `--port-offset 1000`
moves the whole block; the addresses printed at startup are then the correct ones, and the
deployment's own `sbx env` values are not.

**"the sandbox behind this port was recreated".** The container was replaced while you were
connected, so the port now serves a different instance. Restart `sbx connect` to pick up the
new map. This is a refusal on purpose: silently reconnecting would point your open `psql` at
a database that is not the one it started talking to.

---

## Removing sbx

Nothing here is hidden, and all of it is reversible.

```sh
# 1. stop the daemon
launchctl unload ~/Library/LaunchAgents/dev.sbx.daemon.plist   # macOS
systemctl --user disable --now sbx                             # linux
pkill -f 'sbx serve'                                           # or just this

# 2. destroy the sandboxes - THIS DELETES THEIR DATA
sbx list                       # see what exists first
sbx rm <each-sandbox>

# 3. reclaim what is left: snapshots, orphaned volumes
sbx gc --snapshots             # lists, deletes nothing
sbx gc --snapshots --force

# 4. sbx's own state, and the binary
rm -rf ~/.sbx                  # templates, origins, presence and lock files
rm "$(command -v sbx)"
```

**`sbx rm` destroys the sandbox's volume with it.** That is the point - a sandbox is meant
to be cheap to throw away - but it means `sbx rm` on a branch you cared about loses that
branch's database. `sbx snapshot` first if you want it back.

`~/.sbx` holds no data of yours: extracted templates, a record of which spec each sandbox came
from, the daemon's pid file and a lock. Deleting it while sandboxes exist costs you the
`--spec` convenience and nothing else.

---

## On a shared or persistent CI runner

**One daemon per machine, per user.** It owns the public ports for every sandbox on the box,
and a second `sbx serve` refuses to start rather than fighting it.

**`sbx serve --idle 30m &` does not survive a GitHub Actions step.** Each step is a new
shell and the background job dies with it. Start the daemon and use the sandbox **in the same
step**, or install the supervised unit from [`deploy/`](../deploy/) on a self-hosted runner.

Two jobs on one runner share the 20000+ port range and the same daemon. That is fine - they
get different slots - but they are not isolated from each other, and `sbx rm` in one job will
happily destroy the other's sandbox. Name sandboxes after the branch *and* the job if they can
overlap.

---

## Nothing here matches

`sbx doctor --json` is machine-readable, and every number this project publishes has the
script that produced it beside it in [BENCHMARKS.md](BENCHMARKS.md). Issues and patches
welcome.
