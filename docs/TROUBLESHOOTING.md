# When something is wrong

Keyed by what you see. Each entry is symptom → cause → fix, grouped by area. Written for people
and for agents: point a stuck agent here.

Run this first:

```sh
sbx doctor
```

Contents: [Install and doctor](#install-and-doctor) · [The daemon](#the-daemon) ·
[Create](#create) · [Wake](#wake) · [Sleep and data](#sleep-and-data) ·
[Networking and egress](#networking-and-egress) · [MicroVMs](#microvms) ·
[OpenSandbox API and MCP](#opensandbox-api-and-mcp) · [Remote deployments](#remote-deployments) ·
[Kubernetes](#kubernetes) · [Removing sbx](#removing-sbx)

---

## Install and doctor

### "colima is not running" / "the container runtime is not running"

**Cause:** the runtime's socket is not there. sbx names the runtime and the command to start it.

```sh
colima start                 # or: open -a Docker, podman machine start
```

Your sandboxes survive it: stopping the runtime stops them, and the first connection after it
returns wakes them. sbx never starts or stops the runtime itself. If colima stopped without you
asking, `~/.colima/_lima/colima/ha.stderr.log` shows whether something ran `colima stop`.

### "docker did not answer in time"

**Cause:** the runtime is running but not replying. On a loaded colima, listing seven containers
took 1 minute 36 seconds. sbx gives it ten seconds per refresh and reports the timeout rather than
an empty list.

**Fix:** it is the VM, not sbx. Confirm, then free memory or restart the VM:

```sh
time docker ps -a          # if this is slow, everything is
colima status
sbx ui                     # press a: every container on the machine and what it holds
```

`colima restart` stops your containers. sbx sandboxes wake on the next connection; containers
you started by hand do not.

### "`<name>` is preview and off by default"

**Cause:** the command is a gated preview feature (`ssh`, `devcontainer`, `waiting-page`).

**Fix:** turn it on for that command. `sbx features` lists them.

```sh
SBX_FEATURES=ssh sbx ssh <sandbox>
```

### `sbx ui` says a newer version is available

**Cause:** `sbx ui` checks GitHub's releases API at most once a day, in the background. No other
command checks, and CI environments never do.

**Fix:** upgrade, or set `SBX_NO_UPDATE_CHECK=1` to turn the check off. See [CLI.md](CLI.md#update-check).

---

## The daemon

### "connection refused" on the port `sbx env` printed

**Cause, almost always:** no `sbx serve` is running. The ports `sbx env` prints belong to the
daemon, so with no daemon they refuse.

```sh
sbx doctor | grep 'sbx serve'
sbx serve --idle 5m &          # once per machine, not once per sandbox
```

[`deploy/`](../deploy/) has a launchd plist and a systemd unit to run it supervised.

**If a daemon is running:** it finds new sandboxes every `--refresh` (15 s by default). Run
`sbx ready <name>` to wait.

**If the daemon was started with `--only`:** it fronts only the sandboxes its scope names.
`sbx doctor` shows `scoped only: pid N --only osb-`. `sbx create`, `sbx list` and `sbx ui` name
a sandbox that no daemon covers. Start an unscoped daemon, or one whose `--only` covers it.

### `sbx serve` says it is already running

**Cause:** an unscoped daemon already owns this machine's sandbox ports. A second would bind nothing.

**Fix:** if the pid it names is gone, the record is stale and the next start clears it. If it is
alive, you already have a daemon. To run a second one for a subset (a test run, for example),
give it `--only PREFIX`.

### On a shared or persistent CI runner

- **`sbx serve --idle 30m &` does not survive a GitHub Actions step.** Each step is a new shell.
  Start the daemon and use the sandbox in the same step, or install the unit from
  [`deploy/`](../deploy/) on a self-hosted runner.
- **Jobs on one runner share the daemon.** They get different ports but are not isolated:
  `sbx rm` in one job can remove the other's sandbox. Name sandboxes after branch *and* job.
- **`sbx with` removes its sandbox even on failure**, which keeps a runner clean.

---

## Create

### "never became ready within ..."

**Cause:** the service started, but its health command never passed.

**If the message says the command "cannot run in this image":** the health command is not in
the container. It runs *inside*, so `pg_isready` needs postgres tooling and `curl` needs curl:

```sh
docker run --rm --entrypoint sh <image> -c 'command -v pg_isready curl wget'
```

**Otherwise the workload is not coming up.** Read what it printed; `logs` does not wake anything:

```sh
sbx logs <sandbox> <service> --tail 50
```

### The service's config file is a directory inside the container

**Cause:** you declared `files: {"./my.conf": "/etc/thing/my.conf"}`, but the runtime could not
reach the host path, so docker created an empty directory. A VM-backed docker (colima, Docker
Desktop) shares only some host paths: `$HOME` usually, `/var/folders` on macOS usually not.

**Fix:** move the file under your home directory. sbx checks for this after create and says so.

### Two `sbx create` at the same moment fail on a port conflict

**Cause:** two racing creates can pick the same free block of ports. A lock under `~/.sbx` and a
port probe make this rare, not impossible. Two machines driving one remote `DOCKER_HOST` share no lock.

**Fix:** retry. The retry sees the first create's containers and takes the next block. On colima or
Docker Desktop a port forward can outlive its container for a few seconds after `sbx rm`; wait a
moment and retry.

### `sbx create` is slow the more sandboxes exist

It should not be: finding a free block of ports is one API call. If you see it, report it with
`sbx list | wc -l`.

### `sbx list` shows nothing, or a sandbox you cannot remove

**Cause:** `sbx list` is rebuilt from container labels. A container whose `sbx.ports` label does
not parse is skipped, so `list` and `rm` cannot see it.

```sh
docker ps -a --filter label=sbx.sandbox --format '{{.Names}}\t{{.Labels}}'
docker rm -f <name>
```

---

## Wake

### The first query after an idle period fails, but the next one works

**Cause:** your client's connect timeout is shorter than the wake. The wake is paid on `connect`.
Typical wakes are in [BENCHMARKS.md](BENCHMARKS.md): a fraction of a second for redis, about a
second for postgres, several seconds for a cold browser.

**Fix:** raise the connect timeout:

| client | setting |
|---|---|
| libpq / psql | `PGCONNECT_TIMEOUT`, or `connect_timeout=` in the URL |
| JDBC | `connectTimeout` |
| Playwright / Puppeteer | the launch/connect timeout, not the navigation one |

A connection pool must also survive a server-side close: sleeping a sandbox closes its connections.

### Wakes are slower than the numbers in BENCHMARKS.md

- **No `health` command costs a flat 2 s per wake.** sbx cannot tell "port bound" from "ready".
  Add `health`. → [SPEC.md](SPEC.md#health-is-close-to-required)
- **A first wake on a cold machine includes the image pull.** Run `sbx prewarm` first.
- **A wake cannot report faster than `health_interval`** (300 ms by default). Lower it to catch
  readiness sooner. → [SPEC.md](SPEC.md#health_interval-is-what-those-probes-cost)

---

## Sleep and data

### A fork is missing the write I just made

**Cause:** `sbx snapshot` does not stop the service. It takes a crash-consistent copy. Postgres
replays its WAL (write-ahead log) on the fork's start, but under heavy load the copy can catch the WAL mid-write,
and the **last** write before the snapshot can be missing.

**Fix:** if the snapshot must be exact, stop writing first:

```sh
docker stop sbx-<sandbox>-<service>     # or just stop writing to it
sbx snapshot <sandbox> golden
```

The usual seed → snapshot → fork flow has nothing writing at snapshot time.

### `sbx checkpoint` works but `sbx resume` fails

**Cause:** you are on docker, whose checkpoint restore is unmaintained. (Checkpoints use CRIU, a
Linux tool that saves a running process's memory to disk.) Errors look like
`bind-mount /proc/0/ns/net -> …: no such file or directory` or `content … already exists`.
`criu check` passes on the same host.

**Fix:** use podman. Point `DOCKER_HOST` at `unix:///run/podman/podman.sock`; sbx routes
checkpoint and resume through podman. On macOS checkpoint is refused (CRIU needs Linux).
`sbx snapshot` / `fork` work on any runtime.

---

## Networking and egress

### A service with `egress_allow` cannot reach a host

**Cause:** only listed hosts (and their subdomains) are reachable, and only through
`HTTP_PROXY`/`HTTPS_PROXY`. A client that ignores those variables has no route. Raw TCP (`git://`,
SSH, a remote database) never passes the filter.

**Fix:** add the host with `sbx egress <sandbox> --allow <host>`, make the client use the proxy,
or use HTTPS instead of raw TCP. → [SPEC.md](SPEC.md#egress-the-network-a-service-may-reach)

### `sbx egress` says there is no filter to change

**Cause:** only a sandbox created with `egress_policy`, `egress_allow` or `egress: "allow"` has a
filter. **Fix:** add `"egress": "allow"` to the spec, recreate, then narrow it live.

### A sandbox that works inside itself sleeps mid-task

**Cause:** idleness is measured on bytes through the service's ports. Compiling or editing inside
sends none. **Fix:** declare `egress_allow` (its calls out count as activity), a longer `idle`,
or `"idle": "never"`. → [SPEC.md](SPEC.md#idle-keeps-a-sandbox-awake-while-it-works)

---

## MicroVMs

A microVM is a small virtual machine with its own kernel, run by Firecracker when you pass
`--provider firecracker`. Background: [GUIDES.md](GUIDES.md#stronger-isolation-with-microvms).

### `sbx doctor` or `sbx fc backend` refuses firecracker on this host

**Cause:** there is no `/dev/kvm` (Linux's hardware virtualisation device) and no supported
helper VM (the small Linux VM sbx starts on a Mac or Windows to run microVMs in): Linux without
KVM, a Mac older than M3 or macOS 15, or Windows without nested virtualisation. The message names the reason and the fix.
On a Mac chip sbx cannot identify, `SBX_FC_ASSUME_NESTED=1` lets it try.

### A firecracker create is refused: "runs as USER ..."

**Cause:** the image's `USER` is not root. Everything in the VM runs as root, and running a
non-root image as root would drop the boundary it asked for. **Fix:** use `--provider docker`, or
an image whose `USER` is root.

### A microVM fails "making sbxfcN-M's network namespace"

**Cause:** the host cannot make a network namespace or veth pair. With the jailer on (the
default) each VMM (the Firecracker process behind one microVM) is confined by Firecracker's
jailer and runs in its own namespace. It needs iproute2 with `ip netns`, a writable
`/var/run/netns`, and a kernel with `CONFIG_NET_NS` and `CONFIG_VETH`. A container running sbx may
forbid namespaces.

**Fix:** run sbx on the host, or accept the risk with `SBX_FC_JAILER=off` (no jailer, no
namespace; see [SECURITY.md](../SECURITY.md)).

### A microVM fails "mounting the image root (overlay ...)"

**Cause:** the guest kernel has no overlayfs. sbx's pinned kernels have it; a kernel named by
`SBX_FC_KERNEL` may not.

**Fix:** use a kernel with `CONFIG_OVERLAY_FS=y`, or set `SBX_FC_ROOTFS=copy`. Each VM then gets
a whole copy of its image (slower to create without reflink).

### A microVM's workload says "No space left on device"

**Cause:** its writable root layer is full. Its size is `SBX_FC_DISK_SIZE` (10g by default,
sparse).

**Fix:** recreate the sandbox with a larger `SBX_FC_DISK_SIZE` set for `sbx serve` and
`sbx create`, or write the data to a `volume`.

### A microVM "could not sleep: execd did not confirm its seal"

**What it means:** before a microVM sleeps, sbx asks execd (sbx's agent inside the VM, which runs
your commands) to seal itself: stop answering until it is given a new secret. Only then is the
VM's memory saved, so the saved copy never restores already serving. The message means execd did
not confirm in time.

**Cause:** the guest did not answer, almost always because the host is short of memory. sbx asked
three times (10 s, 20 s, 30 s) and took no snapshot. It then gave execd a fresh secret, which
proves it is not sealed, and **left the VM running**. Nothing is lost; the daemon retries on its
next idle check.

The VM is stopped instead in two cases. Its next wake is then a cold boot: the disk is kept, the
memory is not.

- The third failed sleep in a row: the message says "the 3 sleeps in a row it has not".
- The fresh secret could not be given either: the message says "the re-key that would have
  proved it unsealed failed too".

**Fix:** check `sbx doctor` (memory, swap), keep fewer sandboxes awake, or lower `memory` per
microVM.

### `sbx serve --provider firecracker --osb-addr` on a Mac will not start

On an M3+ Mac or Windows the API runs in the helper VM and is fronted on this machine. The front
checks it first and says which check failed:

| message | cause → fix |
|---|---|
| "answered N to a request without the key" | the in-VM API is not keyed. Restart `sbx serve --provider firecracker --osb-addr ...` |
| "rejects the key this machine holds" | the in-VM daemon has another key. Restart `sbx serve` |
| "never answered" | the in-VM daemon did not start. Read its log (below) |
| "--osb-insecure-no-key is refused" / "--osb-pool ... is not carried into the helper VM" | not supported on this path. Drop the flag or `SBX_OSB_POOL` |

```sh
colima ssh --profile sbx-fc -- sudo journalctl -u sbx-fc-serve -n 50
limactl shell sbx-fc sudo journalctl -u sbx-fc-serve -n 50     # lima
```

An endpoint that refuses connections means the mirror could not bind that port here (log:
`cannot open 127.0.0.1:N`). Something else on this machine holds it.

---

## OpenSandbox API and MCP

### The SDK or `sbx mcp` gets 401 `MISSING_API_KEY` or `INVALID_API_KEY`

**Cause:** `sbx serve --osb-addr` serves the OpenSandbox API (an open-source sandbox API whose
SDKs and MCP server sbx supports; see [GUIDES.md](GUIDES.md#opensandbox-sdks)). It always requires
the `OPEN-SANDBOX-API-KEY` header, loopback included ([why](../SECURITY.md#access-and-exposure)).
With no `--osb-key` or `SBX_OSB_KEY`, the key is generated into `~/.sbx/osb/key`.

**Fix:**

```sh
export OPEN_SANDBOX_API_KEY="$(cat ~/.sbx/osb/key)"    # SDKs
sbx mcp                                                # reads the key file itself, loopback only
sbx mcp --url https://osb.example.dev --key "$KEY"     # a remote server
```

### Warm pool creates are slow (the pool always misses)

**Cause:** a warm pool (`--osb-pool`) keeps sandboxes created ahead of time. A create hits it only
if it asks for exactly what the pool was built with; the rules are in
[GUIDES.md](GUIDES.md#opensandbox-sdks) under **Warm pools**.

**Fix:** read the daemon log: each miss is logged as `osb: pool miss for image ...` with the field
that differed. Send the SDK defaults, or drop the custom entrypoint.

### An API sandbox is `Failed` with `runtime_error`

**Cause:** the container stopped before execd (sbx's agent inside it) answered. `status.message`
gives the docker state, exit code, `OOMKilled`, and the engine's start error. Exit 137 with no output is SIGKILL:
out of memory (host or `resourceLimits.memory`) or a `docker kill`. 143 is SIGTERM from outside.

**Fix:** raise `resourceLimits.memory` or free host memory. The same cause is in the daemon log
(`Failed (runtime_error): ...`) and in `sbx history <id>`.

---

## Remote deployments

### `sbx connect` cannot reach a deployment the platform calls healthy

The platform's health check is not the tunnel. Work down this list:

| message or symptom | cause → fix |
|---|---|
| "rejected the token" | `SBX_CONNECT_TOKEN` differs from the deployment's. Update the deployment's copy |
| "the handshake was answered by something that is not this endpoint" | something else answers the URL (login page, router). `curl -sS https://<url>/healthz` answers only if sbx is there |
| "active" but nothing answers | the container died at start. Read its logs. A hand-written image installing sbx `@latest` may lack `--connect-addr`; pin the version as `sbx pack` does |
| "... is http, so SBX_CONNECT_TOKEN would cross the network in the clear" | use the `https://` URL, or `SBX_CONNECT_INSECURE=1` on a network you trust |
| "... came after a flag, where it would have been ignored" | put flags last. The message prints the working line |
| "db and replica both want 127.0.0.1:5432" | two deployments front one port. `--port-offset replica=1000` |
| "cannot open 127.0.0.1:<port>" | your local `sbx serve` owns that port. `--port-offset 1000` |
| "the sandbox behind this port was recreated" | restart `sbx connect` to pick up the new map |

### `sbx ui --connect` shows rows but no cpu or memory

**Cause:** the deployment cannot be metered (a Kubernetes-backed sbx has no `docker stats`), so the
columns read `n/a`. A deployment older than v0.5.0 also has no usage fields.

**Fix:** none needed for Kubernetes. For an old deployment, redeploy with a current `sbx pack`.

### `sbx ui --connect` will not let me wake or remove anything

**Cause:** the deployment runs in **front mode** (`sbx serve --front`). There sbx only forwards
ports to a workload beside it and manages no sandboxes, so wake, sleep, limit, remove and logs are refused.

A deployment that manages sandboxes (`sbx serve --connect-addr` with a provider) accepts all of
them from `sbx ui --connect`, authorised by the connect token.

**Fix:** in front mode, act on the workload where it runs (a shell on that host, or
`kubectl exec`).

---

## Kubernetes

### A create on `--provider kubernetes` is refused

**Cause:** some fields cannot be enforced honestly in a cluster, so they are refused by name:
`build`, `mounts`, `cap_add`, `egress`, `egress_allow`, `egress_policy`, `on_idle: "freeze"`.
`sbx url` is refused too.

**Fix:** name an `image` instead of `build`; use `volume` instead of `mounts`; use a Kubernetes
NetworkPolicy (enforced by your cluster's network plugin) for egress; use an Ingress for a public URL. The full table is in
[SPEC.md](SPEC.md#provider-support).

---

## Removing sbx

```sh
# 1. stop the daemon
launchctl unload ~/Library/LaunchAgents/dev.sbx.daemon.plist   # macOS
systemctl --user disable --now sbx                             # linux
pkill -f 'sbx serve'                                           # or just this

# 2. remove the sandboxes - THIS DELETES THEIR DATA
sbx list
sbx rm <each-sandbox>

# 3. reclaim snapshots and orphaned volumes
sbx gc --snapshots             # lists, deletes nothing
sbx gc --snapshots --force

# 4. microVM helper VM (macOS / Windows), if you used one
sbx fc vm rm --yes

# 5. sbx's own state, and the binary
rm -rf ~/.sbx
rm "$(command -v sbx)"
```

`sbx rm` deletes the sandbox's volume. Run `sbx snapshot` first to keep it.

What `~/.sbx` holds:

| path | what |
|---|---|
| `templates/` | extracted built-in templates |
| `origins/` | which spec each sandbox came from |
| `egress/` | live egress policies set with `sbx egress` |
| `osb/` | the OpenSandbox API key and state |
| `fc/` | microVM disks and state (`SBX_FC_STATE`) |
| `execd/` | the cached in-sandbox agent binary |
| `history.jsonl` | the `sbx history` journal (`SBX_HISTORY`) |
| `daemon.json`, `daemons/`, `slots.lock` | which daemons are running, and the lock that stops two creates taking the same ports |
| `update.json` | the update-check cache |

Deleting it while sandboxes exist loses the live egress policies, the OpenSandbox API key, microVM sandboxes
and history. Container sandboxes keep running.

---

## Nothing here matches

`sbx doctor --json` is machine-readable. Every published number has its script beside it in
[BENCHMARKS.md](BENCHMARKS.md). Issues and patches welcome.
