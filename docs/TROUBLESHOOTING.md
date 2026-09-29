# When something is wrong

Find what you see, then apply the fix. Point a stuck agent here too. Run `sbx doctor` first. If
it reports a missing tool or runtime, `sbx install` installs it: `sbx install gvisor`,
`sbx install checkpoint`, or no name for all it can. It shows each command and asks first;
`--dry-run` only prints. On Docker Desktop or colima, the VM that runs docker owns its config, and
`sbx install` says so instead.

## Install and doctor

### "colima is not running" / "the container runtime is not running"

The runtime's socket is missing. Start it with `colima start`, `open -a Docker` or
`podman machine start`; the message names the one you need.

Your sandboxes survive, and the first connection after the runtime returns wakes them. sbx never
starts or stops the runtime. If colima stopped on its own, `~/.colima/_lima/colima/ha.stderr.log`
shows whether something ran `colima stop`.

### `sbx doctor` lists kata, but a Kata sandbox has no network or will not restart

The `isolation kata` row checks only that dockerd has `kata-runtime` registered. doctor never
runs a Kata container. Kata boots a VM per container, which can fail on a nested or VM host.
Create one sandbox with `--isolation kata` and connect to it before relying on it. Otherwise use
`--isolation gvisor`, or the microVM provider if doctor's `microVM` row allows it.

### "docker did not answer in time"

The runtime is up but too slow. On a loaded colima, listing seven containers took 1 minute 36
seconds, and sbx waits ten seconds per refresh. Confirm with `time docker ps -a` and
`colima status`; in `sbx ui`, press `a` to see every container and what it holds. Then free
memory or restart the VM. `colima restart` stops your containers. sbx sandboxes wake on the next connection; containers
you started by hand do not.

### "`<name>` is preview and off by default"

The command is a preview feature (`ssh`, `devcontainer`, `waiting-page`). Turn it on per command,
as in `SBX_FEATURES=ssh sbx ssh <sandbox>`. `sbx features` lists them.

### `sbx ui` says a newer version is available

`sbx ui` checks GitHub releases at most once a day. No other command checks, and CI never does.
Upgrade, or set `SBX_NO_UPDATE_CHECK=1` ([CLI.md](CLI.md#update-check)).

## The daemon

### "connection refused" on the port `sbx env` printed

Almost always, no `sbx serve` is running, and the daemon owns those ports. Check with
`sbx doctor | grep 'sbx serve'`, then start one per machine with `sbx serve --idle 5m &`.

- To run it supervised, use the launchd plist or systemd unit in [`deploy/`](../deploy/).
- A running daemon finds new sandboxes every `--refresh` (15 s by default). `sbx ready <name>` waits.
- A daemon started with `--only PREFIX` fronts only matching sandboxes. `sbx doctor` shows
  `scoped only: pid N --only osb-`. Start an unscoped daemon, or one whose `--only` covers it.
- Up to v0.15.1, `sbx rm x` then `sbx create x` inside one `--refresh` window could leave the
  daemon serving x's old ports, when the recreate landed on a new slot. `sbx list` shows the new
  ports; `lsof -nP -iTCP -sTCP:LISTEN | grep sbx` shows the old ones. Restart `sbx serve`.
  Fixed after v0.15.1: the daemon notices the new container and serves its ports.

### Other sandboxes went to sleep during `sbx selftest`

Up to v0.15.1, the daemon `sbx selftest` runs in-process adopted every sandbox on the engine,
not just its own. It fought `sbx serve` for their ports ("address already in use" in its log) and
slept them after 3 s idle. They wake on the next connection. If a port stays refused, restart
`sbx serve`. Fixed after v0.15.1: selftest touches only its own `selftest-<pid>` sandbox.

### `sbx serve` says it is already running

An unscoped daemon already owns this machine's sandbox ports. If its pid is gone, the next start
clears the stale record. If it is alive, you already have one; pass `--only PREFIX` for a second.

### On a shared or persistent CI runner

- `sbx serve --idle 30m &` does not survive a GitHub Actions step, since each step is a new shell.
  Start the daemon and use the sandbox in one step, or install the unit from [`deploy/`](../deploy/).
- Jobs on one runner share the daemon. They get different ports, but `sbx rm` in one job can
  remove another's sandbox. Name sandboxes after branch and job.
- `sbx with` removes its sandbox even on failure, including a create that fails partway, which
  keeps a runner clean.

### `sbx with` says the sandbox "already exists"

`sbx with` removes the sandbox it ran against, so it refuses a name that is in use and changes
nothing. Pick an unused name, or run against the existing sandbox without removing it:
`eval "$(sbx env <sandbox>)" && <command>`, or `sbx exec <sandbox> <service> <command>`.

Up to v0.15.1, `sbx with` reused an existing sandbox and then removed it with its volumes, and a
create that failed partway was left behind. Fixed after v0.15.1.

### `sbx with` says the sandbox "is being created or changed by another sbx"

Another `sbx create`, `sbx add` or `sbx with` holds that name right now. `sbx with` does not wait
for it, because it would refuse the name once it exists. Pick another name. If the pid in the
error is not an sbx (`ps -p <pid>`), remove the lock file the error names.

Up to v0.15.1, two `sbx with` of one name started together shared one sandbox, and the first to
finish removed it while the other still ran. Fixed after v0.15.1: the second is refused, and a
teardown removes only the containers its own run made.

### `sbx with` left its sandbox after Ctrl-C or SIGTERM

Up to v0.15.1, SIGINT or SIGTERM ended `sbx with` at once, with status 130 or 143, and left the
sandbox. Remove it with `sbx rm <sandbox>`. Fixed after v0.15.1: the signal is passed on to the
command, the sandbox is removed, and the status is still 130 or 143. A third interrupt leaves the
sandbox, for when you would rather not wait.

## Create

### "never became ready within ..."

The service started but its `health` command never passed.
- If the message says the command "cannot run in this image", the tool is not in the container.
  Check with `docker run --rm --entrypoint sh <image> -c 'command -v pg_isready curl wget'`.
- Otherwise the workload is not coming up. Read `sbx logs <sandbox> <service> --tail 50`, which
  does not wake anything.
- If it ends with "its container is not running: state exited", the workload exited. Its logs
  say why.
- If it says "the runtime could not be asked", docker was not answering, not the service. See
  ["docker did not answer in time"](#docker-did-not-answer-in-time).
- `sbx with --timeout` sets this wait. `sbx create` and `sbx add` wait two minutes.

### The service's config file is a directory inside the container

The runtime could not reach the host path in `files`, so docker created an empty directory. A
VM-backed docker (colima, Docker Desktop) shares `$HOME` but usually not `/var/folders` on macOS.
Move the file under your home directory. sbx checks for this after create and says so.

The check removes that service's container, because every start would mount the same wrong path.
The error lists the services it kept and those it did not reach. Fix the path and re-run the same
`sbx create` to finish the sandbox. A
backend that cannot remove one service stops it instead; then `sbx rm` the sandbox and create it
again.

Up to v0.15.0 that check also fired, wrongly, when `sbx create` was re-run over a sandbox that
was asleep: it could not look inside a stopped container and reported the file as a directory.
If the same path is a regular file once awake (`sbx exec <sandbox> <service> stat -c %F <path>`),
the mount was fine. Fixed after v0.15.0: an asleep service is left as it is, and create says so.

### A spec that validated before is refused at load

From v0.16.0 `sbx validate` and every command that reads a spec refuse, at load, values that used
to fail only at create or reach the container as written:

- `cap_add "NOT_A_CAP" is not a Linux capability` - use a name from `man 7 capabilities`.
- `cap_add "CAP_SYS_PTRACE": write it without the CAP_ prefix` - write `"SYS_PTRACE"`.
- `env ... uses "${X:-y}", which sbx does not expand` - only plain `${NAME}` is substituted;
  compute a default in your shell and reference it as `${NAME}`.
- `memory "lots" is not a size`, `cpu "-1" is not a number of cores`, `gpus "..." is not ...` -
  use `"512m"`, `"0.5"`, `"all"` or `"device=0"`.
- `services depend on each other in a cycle: a → b → a` - remove one `depends_on` edge.

### Two `sbx create` at the same moment fail on a port conflict

Two racing creates can pick the same block of ports. A lock under `~/.sbx` makes this rare, but
two machines sharing one remote `DOCKER_HOST` share no lock. Retry, and the retry takes the next
block. On colima or Docker Desktop a port forward can outlive its container for a few seconds
after `sbx rm`, so wait a moment first.

When a new sandbox's first `docker run` fails with "port is already allocated", create removes
the container it left and tries the next free slot once. If there is none, re-run. Up to v0.15.1
the container stayed in `Created`, and `sbx wake` reported it serving with no network: remove it
with `sbx rm <sandbox>`.

### `sbx create` says the slot lock, or a sandbox, "is still held by pid N"

For 10 minutes another create has either been making its first container (the slot lock) or
creating or changing the same sandbox (its name lock). `ps -p N -o pid,etime,command` shows what
it is doing. If it is not an sbx, remove the lock file the error names and re-run.

Up to v0.15.1 the slot wait gave up after 90 seconds and went ahead without the lock, so creates
queued behind a slow health check could take one slot and fail on its ports. Fixed after v0.15.1.

### `sbx create` warns that a volume "already existed before this create"

A service's data volume is named after its sandbox and service, so a new sandbox takes over one
an earlier sandbox of that name left behind, for example after a fork that failed. The service
starts on that data. To start clean: `sbx rm <sandbox>`, then `docker volume rm <volume>` if it is
still listed, and create again.

### `sbx list` shows nothing, or a sandbox you cannot remove

`sbx list` is rebuilt from container labels. A container whose `sbx.ports` label does not parse
is skipped, so `list` and `rm` cannot see it. Find it with
`docker ps -a --filter label=sbx.sandbox --format '{{.Names}}\t{{.Labels}}'` and remove it with
`docker rm -f <name>`.

## Wake

### `sbx ready` or `sbx wake` says a service "is not running"

The container exited, or never started. The message gives the runtime's state and exit code.
Read `sbx logs <sandbox> <service> --tail 50` for the reason.

Up to v0.15.1, `sbx ready` could report such a service as serving when it had no health check or
docker was slow to answer. Fixed after v0.15.1.

### The first query after an idle period fails, but the next one works

Your client's connect timeout is shorter than the wake. Typical wakes are in
[BENCHMARKS.md](BENCHMARKS.md): under a second for redis, about a second for postgres, several
seconds for a cold browser. Raise the connect timeout:

| client | setting |
|---|---|
| libpq / psql | `PGCONNECT_TIMEOUT`, or `connect_timeout=` in the URL |
| JDBC | `connectTimeout` |
| Playwright / Puppeteer | the launch/connect timeout, not the navigation one |

A connection pool must also survive a server-side close, because sleeping closes connections.

### Wakes are slower than the numbers in BENCHMARKS.md

- No `health` command adds a flat 2 s per wake. Add one ([SPEC.md](SPEC.md#health-is-close-to-required)).
- A first wake on a cold machine includes the image pull. Run `sbx prewarm` first.
- A wake cannot report faster than `health_interval` (300 ms by default)
  ([SPEC.md](SPEC.md#health_interval-is-what-those-probes-cost)).

### A sandbox that works inside itself sleeps mid-task

Idleness is measured on bytes through the service's ports, and compiling or editing inside sends
none. Set `egress_allow` (its calls out count as activity), a longer `idle`, or `"idle": "never"`
([SPEC.md](SPEC.md#idle-keeps-a-sandbox-awake-while-it-works)).

## Sleep and data

### A fork is missing the write I just made

`sbx snapshot` does not stop the service, so it takes a crash-consistent copy. Under heavy load
the last write before the snapshot can be missing. If the snapshot must be exact, stop writing
first, or run `docker stop sbx-<sandbox>-<service>` before `sbx snapshot`. The usual seed,
snapshot, fork flow has nothing writing at snapshot time.

### `sbx snapshot` fails: "the source is empty or does not exist"

Up to v0.15.1 on docker, snapshot fails when any service in the sandbox has no `volume`. That
includes the `web-stack` template's Redis and services added with `sbx add`. The failed snapshot
leaves an image `sbx-snap-<name>-<service>` and empty `sbx-snapvol-*` volumes behind, although
the message says nothing was changed. Snapshot only sandboxes whose services all declare a
`volume`, and delete the leftovers with `sbx snapshot --rm <name>` (or `sbx gc --snapshots --force`).
Fixed after v0.15.1: such a service is saved as its image alone, and a snapshot that fails removes
what it wrote.

### `sbx create` again keeps running the old build

Up to v0.15.1, re-running `sbx create` after editing a `build` context built the new image
`sbx-build-<hash>`, found the container already there, and kept it on the old image while printing
a check mark. `docker inspect --format '{{.Config.Image}}' sbx-<sandbox>-<service>` shows the old
tag. Run `sbx rm` then `sbx create`, which loses the volume's data. Fixed after v0.15.1: create
prints `<service> recreated (image changed)` and keeps the volume.

### `sbx add` put a service on runc in a gVisor or Kata sandbox

Up to v0.15.1, `sbx add` used `--isolation`'s default, `container`, whatever the sandbox was
created with. Fixed after v0.15.1: an added service joins on the sandbox's own tier, and a
different `--isolation` is refused. A sandbox created before then has no record of its tier and
is read as `container`, so pass nothing to `sbx add` there, or recreate the sandbox.

### `sbx checkpoint` works but `sbx resume` fails

You are on docker, whose checkpoint restore is unmaintained. Errors look like
`bind-mount /proc/0/ns/net -> …: no such file or directory` or `content … already exists`, even
though `criu check` passes. Use podman: set `DOCKER_HOST=unix:///run/podman/podman.sock` and sbx
routes checkpoint and resume through it. `sbx snapshot` and `fork` work on any runtime.

On macOS a local engine (a unix socket or a loopback port) runs in a VM, where a checkpoint can
be taken but never restored, so `sbx checkpoint` refuses up front. A Linux daemon reached over
`tcp://` is checked like any other. Up to v0.15.1 it only refused when docker reported
experimental=false. Colima with experimental on took the checkpoint, froze the service, and
failed at resume with the bind-mount error above. `sbx sleep` then `sbx wake` brings such a
service back, cold.

### `sbx resume` says a service was woken after the checkpoint

The service is running, so its checkpoint describes a past it has moved on from. Run
`sbx sleep <sandbox>`, then `sbx resume` again. Up to v0.15.1 on docker, resume reported
"memory and processes intact" here without restoring anything.

## Networking and egress

### A service with `egress_allow` cannot reach a host

Only listed hosts and their subdomains are reachable, and only through `HTTP_PROXY`/`HTTPS_PROXY`.
A client that ignores those variables has no route. Add the host with `sbx egress <sandbox>
--allow <host>`, make the client use the proxy, or switch to HTTPS
([SPEC.md](SPEC.md#egress-the-network-a-service-may-reach)).

### The filter answers 403 "port N of HOST: the egress filter carries ports 80 and 443 only"

**Symptom:** SSH, a database or any other port through the proxy gets 403, even under
`"egress": "allow"`.

**Cause:** the filter carries ports 80 and 443 only. After v0.15.0 that is enforced; before, a
`CONNECT` to any port was tunnelled.

**Fix:** add the port to `egress_allow` as `host:port` (`"github.com:22"`) and run `sbx create`
again. `egress_policy` and `sbx egress` have no port field, so a sandbox that needs another port
uses `egress_allow`.

### 403 "the machine the egress filter runs on, or one behind it"

**Symptom:** a request to `host.docker.internal`, `host.lima.internal`, `172.17.0.1` or another
docker gateway gets 403 although a rule allows it.

**Cause:** on colima and Docker Desktop those addresses are the VM and your Mac. No policy opens
them ([SECURITY.md](../SECURITY.md#containers)).

**Fix:** run what the sandbox needs as a service in the sandbox instead, and reach it by its
service name.

### Under `--isolation gvisor`, `bad address 'sbx-egress:20999'`

**Symptom:** every request from a filtered service fails with `bad address 'sbx-egress:20999'`.

**Cause:** gVisor does not use docker's DNS server on the sandbox's network, and services found
the filter by DNS name.

**Fix:** fixed after v0.15.0: the filter has a fixed address and services find it through
`/etc/hosts`. Recreate the sandbox (`sbx rm`, then `sbx create`): the services need the hosts
entry, which only a new container gets. Service names still do not
resolve under gVisor.

### An edit to `egress_allow` or `egress_policy` did not take effect

**Symptom:** after editing the spec and running `sbx create` again, the old hosts are still
reachable.

**Cause:** up to v0.15.0 the filter was only created with a service's container, so an existing
sandbox kept its old filter.

**Fix:** fixed after v0.15.0: `sbx create` replaces the filter and says so. On an older version,
`sbx rm` the sandbox and create it again.

### An allowed host answers 502 "lookup ...: operation was canceled"

**Symptom:** a host that `sbx egress <sandbox>` lists as `allow` fails with `502 Bad Gateway`
from busybox `wget` (every alpine image), while a denied host correctly gets 403.

**Cause:** that `wget` shuts its write side as soon as its request is sent. Go's HTTP server
cancels a request's context when it sees that EOF, and the filter resolved and dialled on that
context, so the lookup was cancelled before it answered.

**Fix:** fixed after v0.15.0 - the filter now resolves and dials on a context detached from the
client's read side, bounded at 30 s. On an older version, use a client that keeps its connection
open (`curl`, or any language's HTTP library). The filter image is rebuilt from the new source
on the next `sbx create` that needs it. Run `sbx create` again over an existing sandbox and its
filter is replaced with the new build, keeping live changes.

### `sbx egress` says there is no filter to change

Only a sandbox created with `egress_policy`, `egress_allow` or `"egress": "allow"` has a filter.
Add `"egress": "allow"` to the spec, recreate, then narrow it live.

## MicroVMs

For `--provider firecracker` ([GUIDES.md](GUIDES.md#stronger-isolation-with-microvms)).

### `sbx doctor` or `sbx fc backend` refuses firecracker on this host

There is no `/dev/kvm` and no supported helper VM: Linux without KVM, a Mac older than M3 or
macOS 15, or Windows without nested virtualisation. The message names the fix. On a Mac chip sbx
cannot identify, `SBX_FC_ASSUME_NESTED=1` lets it try.

### A firecracker create is refused: "runs as USER ..."

Everything in the VM runs as root, so sbx refuses an image whose `USER` is not root. Use
`--provider docker`, or an image that runs as root.

### A microVM fails "making sbxfcN-M's network namespace"

Each Firecracker process runs in its own network namespace. That needs iproute2 with `ip netns`, a
writable `/var/run/netns`, and a kernel with `CONFIG_NET_NS` and `CONFIG_VETH`. A container
running sbx may forbid namespaces. Run sbx on the host, or accept the risk with
`SBX_FC_JAILER=off` ([SECURITY.md](../SECURITY.md)).

### A microVM fails "mounting the image root (overlay ...)"

The guest kernel has no overlayfs. sbx's pinned kernels have it; one named by `SBX_FC_KERNEL` may
not. Use a kernel with `CONFIG_OVERLAY_FS=y`, or set `SBX_FC_ROOTFS=copy` (a full image copy per
VM, slower to create without reflink).

### A microVM's workload says "No space left on device"

The writable root layer is full. Its size is `SBX_FC_DISK_SIZE` (10g by default, sparse). Recreate
the sandbox with a larger value set for both `sbx serve` and `sbx create`, or write to a `volume`.

### A microVM "could not sleep: execd did not confirm its seal"

Before sleeping, sbx asks its in-VM agent (execd) to lock itself, so a saved VM never restores
already serving. The agent did not confirm, almost always because the host is short of memory.
sbx tried three times, took no snapshot, and left the VM running. Nothing is lost; the daemon
retries on its next idle check.

If the message says "the 3 sleeps in a row it has not" or "the re-key that would have proved it
unsealed failed too", the VM was stopped instead. Its next wake is a cold boot with the disk kept.
Check `sbx doctor` for memory and swap, keep fewer sandboxes awake, or lower `memory` per microVM.

### `sbx exec` on a microVM: "cannot pass stdin"

The in-VM agent has no way to signal end of input, so a command reading piped stdin would never
exit. sbx refuses before running anything. Copy the input in and redirect inside the VM:

```sh
sbx cp my-branch app ./input.sql :/tmp/input.sql
sbx exec my-branch app sh -c 'psql -U app < /tmp/input.sql'
```

Empty stdin (`</dev/null`, a closed pipe) is fine, and `sbx exec -t` types into a command.

### `sbx serve --provider firecracker --osb-addr` on a Mac will not start

On an M3+ Mac or Windows the API runs in the helper VM and is fronted here. The front says which
check failed:

| message | fix |
|---|---|
| "answered N to a request without the key" | restart `sbx serve --provider firecracker --osb-addr ...` |
| "rejects the key this machine holds" | the in-VM daemon has another key; restart `sbx serve` |
| "never answered" | the in-VM daemon did not start; read its log (below) |
| "--osb-insecure-no-key is refused" / "--osb-pool ... is not carried into the helper VM" | not supported here; drop the flag or `SBX_OSB_POOL` |

The in-VM log: `colima ssh --profile sbx-fc -- sudo journalctl -u sbx-fc-serve -n 50`, or with
lima, `limactl shell sbx-fc sudo journalctl -u sbx-fc-serve -n 50`.

An endpoint that refuses connections could not bind that port here (log:
`cannot open 127.0.0.1:N`). Something else on this machine holds it.

## OpenSandbox API and MCP

### The SDK or `sbx mcp` gets 401 `MISSING_API_KEY` or `INVALID_API_KEY`

The OpenSandbox API ([GUIDES.md](GUIDES.md#opensandbox-sdks)) always requires the
`OPEN-SANDBOX-API-KEY` header, loopback included ([why](../SECURITY.md#access-and-exposure)).
Without `--osb-key` or `SBX_OSB_KEY`, the key is generated into `~/.sbx/osb/key`.

```sh
export OPEN_SANDBOX_API_KEY="$(cat ~/.sbx/osb/key)"    # SDKs
sbx mcp                                                # reads the key file itself, loopback only
sbx mcp --url https://osb.example.dev --key "$KEY"     # a remote server
```

### Warm pool creates are slow (the pool always misses)

A create hits the pool only if it matches what the pool was built with
([rules](GUIDES.md#opensandbox-sdks)). Each miss is logged as `osb: pool miss for image ...` with
the field that differed. Send the SDK defaults, or drop the custom entrypoint.

### An API sandbox is `Failed` with `runtime_error`

The container stopped before sbx's in-sandbox agent answered. `status.message` gives the docker
state, exit code, `OOMKilled` and the start error. Exit 137 with no output is SIGKILL: out of
memory (host or `resourceLimits.memory`) or a `docker kill`. 143 is SIGTERM from outside. Raise
`resourceLimits.memory` or free host memory. The daemon log and `sbx history <id>` show the same.

### An API sandbox is `Failed` with `slot_lock_timeout`

Another create on this machine held the slot lock for 10 minutes, so this one placed nothing
rather than choose a slot without it. `status.message` names the holding pid; `ps -p <pid>` shows
what it is doing. If it is not an sbx, remove the lock file the message names and create again.

### An API create on a source build fails with "invalid reference format"

Up to v0.15.1, a build whose version is not a release tag (`v0.15.1-dev+ffd872d`) asked docker for
an activator image of that version, which was never published and is not a valid tag. It also left
an empty `sbx-execd-<version>` volume; remove it with `docker volume rm`. Fixed after v0.15.1: such a
build compiles the agent from its checkout, or asks you to set `SBX_EXECD_BINARY`.

The compile starts with `sbx serve --osb-addr`, so the first create does not wait inside its
ready timeout. The log says `building the sandbox agent` and then `built the sandbox agent ... in`.

## Remote deployments

### `sbx connect` cannot reach a deployment the platform calls healthy

The platform's health check is not the tunnel. Work down this list:

| message or symptom | fix |
|---|---|
| "rejected the token" | `SBX_CONNECT_TOKEN` differs from the deployment's; update it there |
| "the handshake was answered by something that is not this endpoint" | something else answers the URL; `curl -sS https://<url>/healthz` answers only if sbx is there |
| "active" but nothing answers | the container died at start; read its logs. Pin the sbx version as `sbx pack` does |
| "... is http, so SBX_CONNECT_TOKEN would cross the network in the clear" | use `https://`, or `SBX_CONNECT_INSECURE=1` on a trusted network |
| "... came after a flag, where it would have been ignored" | up to v0.15.1: put flags last. Fixed after v0.15.1: flags go anywhere |
| "db and replica both want 127.0.0.1:5432" | `--port-offset replica=1000` |
| "cannot open 127.0.0.1:<port>" | your local `sbx serve` owns that port; `--port-offset 1000` |
| "the sandbox behind this port was recreated" | restart `sbx connect` |

### `sbx ui --connect` shows rows but no cpu or memory

A Kubernetes-backed deployment cannot be metered, so the columns read `n/a`. A deployment older
than v0.5.0 has no usage fields; redeploy it with a current `sbx pack`.

### `sbx ui --connect` will not let me wake or remove anything

The deployment runs in front mode (`sbx serve --front`). It only forwards ports and manages no
sandboxes, so wake, sleep, limit, remove and logs are refused. Act on the workload where it runs
(a shell on that host, or `kubectl exec`).

## Kubernetes

### A create on `--provider kubernetes` is refused

These cannot be enforced in a cluster, so they are refused by name: `build`, `mounts`, `cap_add`,
`egress`, `egress_allow`, `egress_policy`, `on_idle: "freeze"`, and `sbx url`. Use an `image`
instead of `build`, a `volume` instead of `mounts`, a NetworkPolicy for egress, and an Ingress for
a public URL ([SPEC.md](SPEC.md#provider-support)).

## Removing sbx

```sh
launchctl unload ~/Library/LaunchAgents/dev.sbx.daemon.plist   # stop the daemon: macOS
systemctl --user disable --now sbx                             # linux
pkill -f 'sbx serve'                                           # or just this
sbx list; sbx rm <each-sandbox>          # DELETES THEIR DATA; sbx snapshot first to keep one
sbx gc --snapshots --force               # snapshots and orphaned volumes (without --force, lists)
sbx fc vm rm --yes                       # the microVM helper VM, if you used one
rm -rf ~/.sbx && rm "$(command -v sbx)"  # sbx's own state, and the binary
```

Deleting `~/.sbx` while sandboxes exist loses live egress policies, the OpenSandbox API key,
microVM sandboxes and `sbx history`. Container sandboxes keep running.

## Nothing here matches

Open an issue with the output of `sbx doctor --json`.
