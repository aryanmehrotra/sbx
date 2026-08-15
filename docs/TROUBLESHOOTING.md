# When something is wrong

Keyed by what you actually see, not by what is actually wrong — those are different, and the
second one is what this page is for.

Run this first. It answers four of the entries below on its own:

```sh
sbx doctor
```

---

## "connection refused" on the port `sbx env` printed

**Almost always: no `sbx serve` is running.** The ports `sbx env` exports are the daemon's,
not docker's — docker publishes a *backing* port and the daemon owns the public one. Without
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
inside the container — the single most common first-run mistake. A health command runs *in*
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
at the destination** rather than failing. A VM-backed docker — Colima, Docker Desktop — only
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

One daemon serves the whole machine — it owns every sandbox's public ports, so a second one
would bind nothing. If the pid it names is gone, the record is stale and the next start
clears it. If it is alive, you already have what you were about to start.

---

## The first query after an idle period fails, but the next one works

**Your client's connect timeout is shorter than the wake.** The wake is paid on `connect`,
not on the query — the caller sees a slow first connection, and a client configured to give
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
daemon cannot tell "the port is bound" from "the server is ready" — docker binds the host side
of a published port the instant the container starts — so it waits a fixed moment and goes.
Declaring `health` replaces that with an actual check, and is why every bundled template has
one. → [SPEC.md](SPEC.md)

**A first wake on a cold machine includes the image pull.** `sbx prewarm` moves it somewhere
you can cache.

---

## `sbx create` is slow the more sandboxes exist

It should not be any more — creating a sandbox lists the existing ones to pick a free port
slot, and that used to fork a `docker inspect` per container. If you see this on a current
build, it is a bug worth reporting with `sbx list | wc -l`.

---

## Two `sbx create` at the same moment fail on a port conflict

Slot allocation reads which slots are spoken for and takes the first gap, but the ports are
only really claimed when a container binds them — so two creates racing can be handed the same
gap. sbx narrows this from both sides: a lock under `~/.sbx` serialises the claim on one
machine, and `AllocSlot` binds a candidate slot's backing ports before returning it, which is
the same question docker asks a moment later.

Neither closes it completely, and nothing can from inside one process: two machines driving
one remote `DOCKER_HOST` share no lock. Measured on a laptop, four concurrent creates: 1 of 4
succeeded before, 17 of 20 after. **If one fails, retry it** — the retry sees the winner's
containers and takes the next slot.

On a VM-backed docker (Colima, Docker Desktop) the host-side port forward can outlive the
container it belonged to by a few seconds, so a create immediately after an `sbx rm` can
collide with a forward docker already considers gone. Waiting a moment, or retrying, is the
answer there too.

---

## Nothing here matches

`sbx doctor --json` is machine-readable, and every number this project publishes has the
script that produced it beside it in [BENCHMARKS.md](BENCHMARKS.md). Issues and patches
welcome.
