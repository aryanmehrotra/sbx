# When something is wrong

Keyed by what you see, not by what is wrong - the second is what this page is for.

Run this first; it answers four of the entries below on its own:

```sh
sbx doctor
```

---

## "connection refused" on the port `sbx env` printed

**Almost always: no `sbx serve` is running.** The ports `sbx env` exports are the daemon's, not
docker's - docker publishes a *backing* port, the daemon owns the public one, so an address with
no daemon behind it refuses.

```sh
sbx doctor | grep 'sbx serve'
sbx serve --idle 5m &          # once per machine, not once per sandbox
```

`sbx create` checks this. [`deploy/`](../deploy/) has a launchd plist and a systemd unit to run
it supervised, so it survives your terminal.

**If a daemon *is* running:** it discovers new sandboxes on its `--refresh` interval (15 s by
default), so one created seconds ago may not be fronted yet. `sbx ready <name>` waits.

---

## "colima is not running" / "the container runtime is not running"

Its socket is not there. sbx names the runtime from where that socket was and gives you the
command:

```sh
colima start                 # or: open -a Docker, podman machine start
```

**Your sandboxes survive it.** They are containers with volumes; stopping the runtime stops
them, and the first connection after it returns wakes them again. Nothing is lost.

sbx never starts or stops the runtime itself. If yours stopped without you asking,
`~/.colima/_lima/colima/ha.stderr.log` records whether something ran `colima stop` - a clean
stop, not a crash - before you go looking for a bug here.

---

## "docker did not answer in time"

The runtime is running and not replying. Measured on a loaded colima: **1 minute 36 seconds** to
list seven containers, versus milliseconds on an idle one. sbx gives it ten seconds per refresh
and says so rather than reporting an empty fleet - a timed-out listing and an empty machine are
different answers.

It is the VM, not sbx - every command waits on that same daemon, so all are slow together.
Confirm by hand:

```sh
time docker ps -a          # if this is slow, everything is
colima status              # or Docker Desktop's own dashboard
```

Usual causes: the machine out of memory or cpu - `sbx ui`, press `a` for what is on it - or the
VM up long enough to want restarting. `colima restart` stops your containers; sbx sandboxes
survive it and wake on the next connection, but anything you started by hand does not.

---

## "never became ready within 2m0s"

The service started and its health command never passed.

**If the message names the command and says "cannot run in this image"**, the command is not
inside the container - the most common first-run mistake. A health command runs *in* the
container, so `pg_isready` needs postgres tooling in that image and `curl` needs curl:

```sh
docker run --rm --entrypoint sh <image> -c 'command -v pg_isready curl wget'
```

**Otherwise the workload really is not coming up.** Look at what it said:

```sh
sbx logs <sandbox> <service> --tail 50
```

`logs` is the one command that wakes nothing, so this is safe on a sleeping sandbox.

---

## The service's config file is a directory inside the container

You declared `files: {"./my.conf": "/etc/thing/my.conf"}` and the container behaves as if the
file is empty or missing.

**The runtime could not reach the host path, so docker created an empty directory at the
destination** rather than failing. A VM-backed docker - Colima, Docker Desktop - shares only some
host paths; `/var/folders` on macOS usually is not one, `$HOME` usually is.

sbx checks for this after create and says so by name. Move the file under your home directory.

---

## `sbx list` shows nothing, or a sandbox you cannot remove

`sbx list` is rebuilt from labels on the containers. A container whose `sbx.ports` label will not
parse is skipped, so it is invisible to `list` and `rm`.

```sh
docker ps -a --filter label=sbx.sandbox --format '{{.Names}}\t{{.Labels}}'
docker rm -f <name>            # the escape hatch; sbx is not hiding anything from you
```

---

## `sbx serve` says it is already running

One daemon serves the whole machine - it owns every sandbox's public ports, so a second would
bind nothing. If the pid it names is gone, the record is stale and the next start clears it; if
it is alive, you already have what you were about to start.

---

## The first query after an idle period fails, but the next one works

**Your client's connect timeout is shorter than the wake.** The wake is paid on `connect`, not
the query, so a client set to give up in two seconds will.

Typical wakes: ~191 ms for redis, a second or so for postgres, several seconds for a browser on
a cold cache. Raise the connect timeout above that:

| client | knob |
|---|---|
| libpq / psql | `PGCONNECT_TIMEOUT`, or `connect_timeout=` in the URL |
| JDBC | `connectTimeout` |
| Playwright / Puppeteer | the launch/connect timeout, not the navigation one |

A pooled client must also tolerate a server-initiated close: sleeping a sandbox closes the
connections it holds.

---

## Wakes are slower than the numbers in BENCHMARKS.md

**A service with no `health` command costs a flat 2 s per wake.** With nothing to probe - docker
binds the host side the instant the container starts - the daemon cannot tell "port bound" from
"server ready", so it waits a fixed moment and goes. Declaring `health` replaces that with a real
check, and is why every bundled template has one. → [SPEC.md](SPEC.md)

**A first wake on a cold machine includes the image pull.** `sbx prewarm` moves it somewhere you
can cache.

**A wake cannot be reported faster than the probe interval**, re-evaluated only every
`health_interval` - 300 ms by default. A service ready in 40 ms reports as 300 ms, or a second if
you set `1s`. Turn it down to catch readiness quickly; turn it up, sandbox-wide, to spend less of
the machine probing a mostly-idle fleet. → [SPEC.md](SPEC.md)

---

## `sbx create` is slow the more sandboxes exist

It should not be. Creating a sandbox lists the existing ones to pick a free port slot; that used
to fork a `docker inspect` per container - it is one API call now. If you still see it, that is a
bug worth reporting with `sbx list | wc -l`.

---

## Two `sbx create` at the same moment fail on a port conflict

Slot allocation reads which slots are spoken for and takes the first gap, but ports are only
really claimed when a container binds them - so two racing creates can be handed the same gap.
sbx narrows this from both sides: a lock under `~/.sbx` serialises the claim on one machine, and
`AllocSlot` binds a candidate slot's backing ports before returning it.

Neither closes it completely: two machines driving one remote `DOCKER_HOST` share no lock.
Measured on a laptop, four concurrent creates repeated five times: 5 of 20 succeeded before, 17
of 20 after. **If one fails, retry it** - the retry sees the winner's containers and takes the
next slot.

On a VM-backed docker (Colima, Docker Desktop) the host-side port forward can outlive its
container by a few seconds, so a create right after an `sbx rm` can collide with a forward docker
already considers gone. Waiting a moment, or retrying, is the answer there too.

---

## A fork is missing the write I just made

`sbx snapshot` does **not** stop the service first. It takes a crash-consistent copy - the state
the database would recover from after a power cut - because stopping would silently interrupt
whoever is using the sandbox.

Databases normally survive that: an acknowledged Postgres commit is already in the WAL, and the
fork replays it on start. But the copy can catch the WAL mid-write, and Postgres stops replay at
the first torn record - so under heavy load the **last** write before the snapshot can be missing.
Seen once here, under several suites competing for one docker daemon.

**If the snapshot must be exact**, quiesce it first:

```sh
docker stop sbx-<sandbox>-<service>     # or just stop writing to it
sbx snapshot <sandbox> golden
```

For the ordinary case - seed, snapshot, fan out - nothing is writing at snapshot time and this
does not arise.

---

## `sbx ui --connect` shows rows but no cpu or memory

**The deployment's backend cannot be metered.** Usage is optional, as it is locally: a
kubernetes-backed sbx has no `docker stats` to call, so the columns read `n/a` rather than a zero
nobody measured. A docker-backed deployment fills them.

**Or the deployment is older than v0.5.0.** The usage fields are new; an older `sbx serve` answers
the same listing without them, so every row reads `n/a`. `sbx pack` pins the version, so redeploy
to move it forward.

Sampling is done only when asked (`?stats=1`), so a plain `sbx connect` costs the deployment what
it always did - one listing, no per-container round trips.

---

## `sbx ui --connect` will not let me wake or remove anything

**That is deliberate, not a missing feature.** A connect endpoint's token buys two things: read
what is fronted, and carry bytes to a port. Waking, sleeping, capping and removing are none of
those, and a leaked token is a far worse incident if it can also destroy a sandbox's volume.

Run the command where the sandbox is - a shell on that host, or `kubectl exec` - and the
dashboard there has every key.

---

## `sbx connect` cannot reach a deployment the platform calls healthy

The platform's health check and the tunnel are not the same fact: a container can be up and serve
nothing that answers `sbx connect`. Work down this list - ordered by how often each was the answer
while this was built.

**"rejected the token".** `SBX_CONNECT_TOKEN` here is not the one the deployment was given. It is
set in two places, and the deployment's copy is the one people forget after a redeploy.

**"the handshake was answered by something that is not this endpoint".** Something is in front of
sbx - a platform login page, a router sending `/` elsewhere, or a URL belonging to a different
service. Check `curl -sS https://<url>/healthz`: that route needs no token and answers only if
sbx itself is on the other end.

**The deployment is "active" but nothing answers at all.** Almost always the container died at
startup and the platform restarted it quietly. Read its logs. The one that catches people is a
hand-written Dockerfile installing sbx with `@latest`: releases before this feature have no
`--connect-addr`, so the process exits on an unknown flag. `sbx pack` pins the version for exactly
this reason - if you wrote the image yourself, pin it too.

**"... is http, so SBX_CONNECT_TOKEN would cross the network in the clear".** The token is the
whole of the security, and `http://` sends it as text to anything in between. Use the `https://`
URL the platform gave you. `SBX_CONNECT_INSECURE=1` waives it for a network you trust, and a
loopback address never needed it - a `kubectl port-forward` or local daemon is exempt already.

**"... came after a flag, where it would have been ignored".** Flags go last. `sbx connect db=…
--port-offset 1000 cache=…` would otherwise connect `db` alone and never mention `cache` - a port
map with a hole in it: whatever else answers on the missing port, often this machine's own `sbx
serve`, gets the connection instead. The message prints the line that would have worked.

**"db and replica both want 127.0.0.1:5432".** Two deployments are fronting the same port - what
happens when a sandbox has two of the same service. They cannot share one local port, so move
one: `--port-offset replica=1000`. The listing then says which was shifted, because its own `sbx
env` values no longer apply here.

**"cannot open 127.0.0.1:<port>".** This machine's own `sbx serve` already owns that port, because
the deployment hands out the same numbers it would locally. `--port-offset 1000` moves the whole
block; the addresses printed at startup are then correct, and the deployment's own `sbx env`
values are not.

**"the sandbox behind this port was recreated".** The container was replaced while you were
connected, so the port now serves a different instance. Restart `sbx connect` to pick up the new
map. A deliberate refusal: silently reconnecting would point your open `psql` at a database that
is not the one it started talking to.

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

**`sbx rm` destroys the sandbox's volume with it.** That is the point - a sandbox is meant to be
cheap to throw away - but `sbx rm` on a branch you cared about loses that branch's database. `sbx
snapshot` first if you want it back.

`~/.sbx` holds no data of yours: extracted templates, a record of which spec each sandbox came
from, the daemon's pid file and a lock. Deleting it while sandboxes exist costs you the `--spec`
convenience and nothing else.

---

## On a shared or persistent CI runner

**One daemon per machine, per user.** It owns the public ports for every sandbox on the box, and
a second `sbx serve` refuses to start rather than fight it.

**`sbx serve --idle 30m &` does not survive a GitHub Actions step.** Each step is a new shell and
the background job dies with it. Start the daemon and use the sandbox **in the same step**, or
install the supervised unit from [`deploy/`](../deploy/) on a self-hosted runner.

Two jobs on one runner share the 20000+ port range and the same daemon. That is fine - they get
different slots - but they are not isolated, and `sbx rm` in one job will happily destroy the
other's sandbox. Name sandboxes after the branch *and* the job if they can overlap.

---

## Nothing here matches

`sbx doctor --json` is machine-readable, and every number this project publishes has the script
that produced it beside it in [BENCHMARKS.md](BENCHMARKS.md). Issues and patches welcome.
