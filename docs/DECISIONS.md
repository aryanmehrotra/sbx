# Decisions

> **Short version:** why sbx is shaped the way it is, and why not the obvious alternative. If
> you are about to change how sandboxes are addressed, woken or slept, "why not just..." is
> probably answered here.

Each entry states a decision and the reasoning behind it — including, where it matters, why the
obvious alternative doesn't hold up. Entries are records: their headings never change (other
pages link to them), and old entries keep their wording. New entries follow
[AGENTS.md](../AGENTS.md#writing-docs).

## Index

Grouped by area. "Recorded" is the first release whose DECISIONS.md carried the entry, which
can be later than the behaviour itself. Status is *current* unless the entry has been amended; an
amended entry carries a **Status:** line under its heading.

| Decision | Area | Recorded | Status |
|---|---|---|---|
| [There is no `start` and no `stop`](#there-is-no-start-and-no-stop) | Lifecycle and wake | v0.1.0 | amended v0.7.0 |
| [Bytes, not connections](#bytes-not-connections) | Lifecycle and wake | v0.1.0 | current |
| [Ask the workload, not the platform](#ask-the-workload-not-the-platform) | Lifecycle and wake | v0.1.0 | current |
| [A published port is not readiness](#a-published-port-is-not-readiness) | Lifecycle and wake | v0.1.0 | current |
| [A sandbox cannot sleep until it has been seen serving](#a-sandbox-cannot-sleep-until-it-has-been-seen-serving) | Lifecycle and wake | v0.1.0 | current |
| [Stops run off the reaper, and a listing that contradicts the daemon is asked again](#stops-run-off-the-reaper-and-a-listing-that-contradicts-the-daemon-is-asked-again) | Lifecycle and wake | unreleased | current |
| [The machine's daemon leaves a live `--only` daemon's sandboxes to it](#the-machines-daemon-leaves-a-live---only-daemons-sandboxes-to-it) | Lifecycle and wake | unreleased | current |
| [Slots are allocated, not hashed](#slots-are-allocated-not-hashed) | Addressing and slots | v0.1.0 | amended v0.10.0 |
| [Optional services still reserve their ports](#optional-services-still-reserve-their-ports) | Addressing and slots | v0.1.0 | current |
| [128 docker slots, bounded by the ephemeral range](#128-docker-slots-bounded-by-the-ephemeral-range) | Addressing and slots | v0.10.0 | current |
| [The API's creates hold the slot lock for the choice only](#the-apis-creates-hold-the-slot-lock-for-the-choice-only) | Addressing and slots | v0.10.0 | current |
| [Three containers, not one image with everything in it](#three-containers-not-one-image-with-everything-in-it) | Spec, images and templates | v0.1.0 | current |
| [A built image is keyed by its content, never by its age](#a-built-image-is-keyed-by-its-content-never-by-its-age) | Spec, images and templates | v0.1.0 | current |
| [Adding an optional spec field does not bump `version`](#adding-an-optional-spec-field-does-not-bump-version) | Spec, images and templates | v0.1.0 | current |
| [Template images are pinned by digest, and the pin has a visible date](#template-images-are-pinned-by-digest-and-the-pin-has-a-visible-date) | Spec, images and templates | v0.1.0 | current |
| [`prewarm` is a separate command, not something `create` does](#prewarm-is-a-separate-command-not-something-create-does) | Spec, images and templates | v0.1.0 | current |
| [sbx is a tool people run, not a service anyone offers](#sbx-is-a-tool-people-run-not-a-service-anyone-offers) | Scope, isolation and trust | v0.1.0 | current |
| [Loopback is not a trust boundary on a VM-backed engine](#loopback-is-not-a-trust-boundary-on-a-vm-backed-engine) | Scope, isolation and trust | v0.10.0 | current |
| [Isolation fails closed, and says why](#isolation-fails-closed-and-says-why) | Scope, isolation and trust | v0.1.0 | current |
| [A sandbox has one isolation tier](#a-sandbox-has-one-isolation-tier) | Scope, isolation and trust | unreleased | current |
| [Capabilities are negotiated, not stubbed - and sbx does not reach around a provider](#capabilities-are-negotiated-not-stubbed---and-sbx-does-not-reach-around-a-provider) | Scope, isolation and trust | v0.1.0 | current |
| [Tunnels are shelled out, and the anonymous one is opt-in](#tunnels-are-shelled-out-and-the-anonymous-one-is-opt-in) | Scope, isolation and trust | v0.1.0 | current |
| [A snapshot is the volume, not the container](#a-snapshot-is-the-volume-not-the-container) | Snapshots and checkpoints | v0.1.0 | current |
| [Memory checkpoint goes through podman, because docker's restore path doesn't work](#memory-checkpoint-goes-through-podman-because-dockers-restore-path-doesnt-work) | Snapshots and checkpoints | v0.7.0 | current |
| [Egress is denied by a bridge without NAT, not by a firewall sbx writes](#egress-is-denied-by-a-bridge-without-nat-not-by-a-firewall-sbx-writes) | Egress | v0.1.0 | amended v0.7.0–v0.9.0 |
| [Traffic through the egress filter counts as activity, stamped on bytes](#traffic-through-the-egress-filter-counts-as-activity-stamped-on-bytes) | Egress | v0.8.0 | current |
| [A live egress policy is held by the filter and pushed to it](#a-live-egress-policy-is-held-by-the-filter-and-pushed-to-it) | Egress | v0.9.0 | current |
| [Default-allow is enforced by the same door, and it costs raw TCP](#default-allow-is-enforced-by-the-same-door-and-it-costs-raw-tcp) | Egress | v0.9.0 | current |
| [An API snapshot is the container, because that is where its state is](#an-api-snapshot-is-the-container-because-that-is-where-its-state-is) | OpenSandbox API | v0.10.0 | current |
| [Volumes on the API: host paths are the operator's to allow, and claims are namespaced](#volumes-on-the-api-host-paths-are-the-operators-to-allow-and-claims-are-namespaced) | OpenSandbox API | v0.10.0 | current |
| [Warm-pool members wait pinned running, and every claim re-keys](#warm-pool-members-wait-pinned-running-and-every-claim-re-keys) | OpenSandbox API | v0.10.0 | current |
| [An API sandbox's health check runs once a minute, and quickly only while it starts](#an-api-sandboxs-health-check-runs-once-a-minute-and-quickly-only-while-it-starts) | OpenSandbox API | v0.10.0 | current |
| [What the API remembers lives in its record file, not in labels](#what-the-api-remembers-lives-in-its-record-file-not-in-labels) | OpenSandbox API | v0.10.0 | current |
| [An API sandbox freezes when idle; a sandbox.json sandbox stops](#an-api-sandbox-freezes-when-idle-a-sandboxjson-sandbox-stops) | OpenSandbox API | v0.10.0 | current |
| [The OpenSandbox API is not on a cluster, for now](#the-opensandbox-api-is-not-on-a-cluster-for-now) | OpenSandbox API | v0.12.0 | current |
| [The OpenSandbox API on a microVM: the agent is PID 1, the token is the API's, a snapshot is the disk](#the-opensandbox-api-on-a-microvm-the-agent-is-pid-1-the-token-is-the-apis-a-snapshot-is-the-disk) | OpenSandbox API | v0.12.0 | amended v0.13.x |
| [The OpenSandbox API through a helper VM runs in the VM](#the-opensandbox-api-through-a-helper-vm-runs-in-the-vm) | OpenSandbox API | v0.13.0 | current |
| [A microVM off Linux runs in a helper VM, not on Virtualization.framework](#a-microvm-off-linux-runs-in-a-helper-vm-not-on-virtualizationframework) | MicroVM (firecracker) | v0.11.0 | current |
| [One host probe decides the microVM path, and a Mac is sent to a helper VM, not refused](#one-host-probe-decides-the-microvm-path-and-a-mac-is-sent-to-a-helper-vm-not-refused) | MicroVM (firecracker) | v0.11.0 | current |
| [A microVM's root filesystem comes from docker export, not from layers](#a-microvms-root-filesystem-comes-from-docker-export-not-from-layers) | MicroVM (firecracker) | v0.11.0 | current |
| [The image rootfs holds the image; the agent rides on its own drive](#the-image-rootfs-holds-the-image-the-agent-rides-on-its-own-drive) | MicroVM (firecracker) | v0.11.0 | amended v0.14.0 |
| [A microVM links its image and writes to a layer of its own](#a-microvm-links-its-image-and-writes-to-a-layer-of-its-own) | MicroVM (firecracker) | v0.14.0 | current |
| [A snapshot is invalid from the moment its VM runs](#a-snapshot-is-invalid-from-the-moment-its-vm-runs) | MicroVM (firecracker) | v0.11.0 | current |
| [A sleep whose seal is not confirmed keeps the VM, re-keyed, and a guest cannot keep it for ever](#a-sleep-whose-seal-is-not-confirmed-keeps-the-vm-re-keyed-and-a-guest-cannot-keep-it-for-ever) | MicroVM (firecracker) | v0.14.0 | current |
| [A firecracker snapshot restores only as itself](#a-firecracker-snapshot-restores-only-as-itself) | MicroVM (firecracker) | v0.11.0 | current |
| [Guest networking is arithmetic, and has no way out](#guest-networking-is-arithmetic-and-has-no-way-out) | MicroVM (firecracker) | v0.11.0 | amended v0.12.0 |
| [A microVM's only door is its filter, and the host behind it is closed](#a-microvms-only-door-is-its-filter-and-the-host-behind-it-is-closed) | MicroVM (firecracker) | v0.12.0 | amended v0.13.0 |
| [Every VMM runs under Firecracker's jailer, as its VM's own uid, jailed inside its VM's directory](#every-vmm-runs-under-firecrackers-jailer-as-its-vms-own-uid-jailed-inside-its-vms-directory) | MicroVM (firecracker) | v0.13.0 | amended v0.14.0 |
| [Warm-pool members wait asleep on a microVM, and each is its own VM](#warm-pool-members-wait-asleep-on-a-microvm-and-each-is-its-own-vm) | MicroVM (firecracker) | v0.13.0 | current |
| [What makes two wake numbers comparable](#what-makes-two-wake-numbers-comparable) | Measuring | v0.1.0 | current |

---

## Lifecycle and wake

### There is no `start` and no `stop`

**Status:** current, amended in v0.7.0: `sbx wake` and `sbx sleep` override one transition on request; the daemon still owns the idle policy (ARCHITECTURE.md, "The exceptions, named").

Whatever can start a sandbox becomes the thing that left one running, and then two components
believe they own the lifecycle. Only the daemon owns it, because only it can see demand. So the
build harness integration has a readiness predicate and no `up`: **asking is starting**, leaving
nothing for a second component to own.

### Bytes, not connections

A Go service's pool holds sockets open indefinitely, so a sandbox fronted by a running service
is never idle by connection count and never sleeps.

### Ask the workload, not the platform

Docker republishes container health only on its check interval, which lags the workload itself —
measured at **98% of a wake**: 5030 ms against a Redis serving in 110 ms. So the wake path runs
the declared health command itself, rather than asking docker; the reaper still asks the cheap,
lagging question, because being seconds late to *sleep* something costs nothing.

### A published port is not readiness

Docker binds the host side of `-p` the instant a container starts, before the process behind it
is ready to serve — measured at **139 ms** for the port to answer against about a second more
before the server itself was ready, with a client spliced in between dying while reading the
handshake. So services declare a health check, and the daemon asks the container rather than
trusting the port.

### A sandbox cannot sleep until it has been seen serving

"Idle" is meaningless before a service has ever been up: a sandbox pulling an image and running
migrations looks exactly like one nobody has touched. Left ungated, that reading is reachable
fast — scaling to zero **39 seconds into creation**, while create is still waiting on the first
health check, is well within range. So a sandbox is not eligible to sleep until it has been seen
serving at least once.

### Stops run off the reaper, and a listing that contradicts the daemon is asked again

The reaper decides on its own clock and runs each stop on a goroutine of its own. A stop is slow:
docker waits a **10 s** grace for a workload that ignores SIGTERM, and a busybox `sh -c` loop does.
Run inline, one such stop held the reaper and the discovery tick for those ten seconds, so a
service with `"idle": "3s"` slept **14 s** after its last byte in three runs out of three. Measured
after the change on the same machine: stopped **3.2-4.0 s** after the last byte, then the grace.
A unit already on its way down is not stopped again, and it keeps its dependencies up until its
stop returns, which is the top-down order the inline reaper got by blocking.

Discovery lists every sandbox and revokes "awake" from any the provider reports stopped. The
listing can be older than a wake that finished after it. A connection that arrived mid-stop woke
the service in 175 ms, and 17 ms later the tick applied its stale "not running": the container ran
while the daemon believed it asleep, and the reaper, which only looks at awake units, never slept
it again. So a listing that contradicts the daemon is asked again under the wake lock, where no
wake or stop can move the unit. The obvious alternative, trusting the lock alone, proves only that
no wake is in flight now, not that none finished since the listing. Asking again costs a call
only on a contradiction, never for the sleeping majority.

### The machine's daemon leaves a live `--only` daemon's sandboxes to it

An unscoped `sbx serve` adopted every sandbox, including those a running `--only` daemon covered,
so two daemons raced to bind the same ports. The loser logged "address already in use" every
`--refresh`, and which one fronted the sandbox was an accident of timing. So the unscoped daemon
reads `~/.sbx/daemons` on every discovery pass and skips what a live scoped daemon covers. Every
pass, not once at start, so a scoped daemon that stops hands its sandboxes back within one tick.
Live means its pid exists, and a dead daemon's record is deleted when read, so a killed `--only`
daemon hides nothing.


---

## Addressing and slots

### Slots are allocated, not hashed

**Status:** current. The 60-slot cap it mentions was raised to 128 in v0.10.0; see "128 docker slots, bounded by the ephemeral range".

Hashing branch names into 60 slots looks stable and collides in practice — `auth-flow` and
`naveen-reveiw` collide within the first six names tried. Two sandboxes on one slot fight over
ports, so slots are allocated instead, with docker labels as the registry: nothing can drift from
reality because there is no derived mapping to drift from.

### Optional services still reserve their ports

Skipping an optional service must not shift the ordinal of the ones after it — adding ClickHouse
later would otherwise move the database out from under every config that had recorded where it
was. So a service's ordinal is fixed whether or not it is actually present.

### 128 docker slots, bounded by the ephemeral range

60 slots held a laptop's branches. A warm pool plus ComputeSDK's burst of a hundred creates needs
more than 60 sandboxes on one engine at once, so the cap is 128. The ceiling is not arbitrary:
backing ports run from 30000 in blocks of 20, and they must stay below Linux's default ephemeral
range (32768+) or an outgoing connection on the host can hold a port docker is about to publish -
a failure that arrives at random, not at once. 30000 + 128×20 = 32560. Public ports run
20000-22559. `TestBackingRangeStaysBelowTheEphemeralPorts` holds the arithmetic.

### The API's creates hold the slot lock for the choice only

The machine-wide slot lock used to be held from reading the container list until the new
container existed - correct for one create, a queue for a hundred: each waited for every earlier
create's list and `docker run`, the hundredth for about a minute. The OpenSandbox API now holds it
only while choosing: slots it has handed to creates still in `docker run` are reserved in-process
(until a minute after the container exists, so a list that began earlier cannot miss it), and at
most 8 `docker run`s are in flight.

Accepted, not closed: an `sbx create` on the CLI choosing a slot while the daemon is mid-`docker
run` can now pick the same slot, where before it waited. It fails at `docker run` on the port -
the port probe in the choice narrows this, as it already did for two machines on one remote
engine - and a retry takes the next slot (TROUBLESHOOTING.md). Closing it would mean the CLI
asking the daemon for a slot, a protocol this does not add.


---

## Spec, images and templates

### Three containers, not one image with everything in it

One image is simpler to reason about, and wrong here. Once waking is automatic, splitting is
*cheaper*: a branch that never queries the analytics store never pays for it. Merging them means
waking ClickHouse to read a config row.

### A built image is keyed by its content, never by its age

`build:` names the image it produces `sbx-build-<sha256 of the context>`, so the second create
with an unchanged context finds the image already there and does no build at all.

The alternative — and what [Daytona documents][daytona-builder] — is to expire the cache on a
timer: "Declarative images are cached for 24 hours ... subsequent runs **on the same runner**
will be almost instantaneous." Note the last clause; a content hash does not care which runner it
is on. A clock is wrong in both directions at once: it rebuilds a context unchanged since
yesterday, and reuses one that changed five minutes ago if the entry is still young.
Content-addressing has neither failure — change a byte and the tag changes; change nothing and
the tag is the same next month.

What goes into the hash decides whether this works in practice:

**Not timestamps.** A fresh `git clone` rewrites every mtime, so an mtime-keyed cache would miss
on every CI runner — precisely the machine where it is worth the most, and the one where the
developer never sees it failing.

**File modes, yes.** A script that stops being executable is a different image. Hashing only
contents would make that a silent cache hit that fails at runtime, worse than a rebuild.

**`.git` and `node_modules`, no.** Otherwise the tag would change on every commit whether or not
any build input did, and the cache would never hit twice.

**Symlinks are skipped**, since the target is either already inside the context or outside it,
and following one out would put the host's filesystem into the key.

`image` and `build` together is an error rather than a precedence rule: whichever we picked, half
of all readers would guess the other, and the cost of guessing wrong is running an image the file
does not appear to describe.

Docker only. Building in a cluster means pushing to a registry the nodes can pull from —
credentials, a registry address, a retention policy — none of which sbx can assume without
becoming an opinionated CI system. `BuilderFor` refuses on kubernetes and says why, the same
negotiated-capability rule as snapshots.

### Adding an optional spec field does not bump `version`

The tempting rule is that every new field is a new format version. It is wrong here, because
`ParseSpec` already sets `DisallowUnknownFields`. An older binary meeting a newer spec says:

```
sbx: sandbox.json: json: unknown field "build"
```

which names the field that is not understood. Bumping to `"version": 2` would replace that with
`unsupported version 2 (this build understands 1)` — strictly less information — and would force
every existing spec file to be edited even when it uses nothing new.

So `version` is reserved for a change that would be **silently misread**: a field that changes
meaning, a default that flips, a structure that is re-shaped. Optional additions are not that,
and the decoder already refuses them by name.

### Template images are pinned by digest, and the pin has a visible date

A mutable tag like `zenika/alpine-chrome:latest` would mean the first thing a new user runs could
break without a commit touching this repo, and the failure would read as "sbx is broken" rather
than "the upstream image moved". So every template image is pinned as `name:tag@sha256:...`.

The tag is kept beside the digest deliberately: docker resolves by digest and ignores the tag, so
it costs nothing and tells a reader what they are running, where a bare digest tells them nothing.

**A digest is only accepted if it names a manifest list covering linux/amd64 and linux/arm64.**
`scripts/pin-templates.sh` refuses otherwise. An arch-specific manifest resolved on a laptop
would produce templates that pull there and fail in CI, and that failure would look like a broken
template rather than a bad pin — the worst kind, because it sends whoever hits it to the wrong
file.

Pinning buys reproducibility and pays for it in staleness: these images stop receiving updates
until somebody refreshes them. That is only an honest trade if the age is visible, so
`sbx templates` prints the refresh date and `examples/pinned.json` carries it. A pin whose age
nobody can see is a pin nobody ever refreshes.

There is no `make templates-refresh` because there is no Makefile — this repo puts its tooling in
`scripts/`, and adding a build system for one target would be the second way to do something.

### `prewarm` is a separate command, not something `create` does

The first create on a cold machine is mostly a download. Folding a pull into create would make
every create's timing depend on whether the machine happened to be warm — exactly the ambiguity
the wake measurements exist to avoid.

`sbx prewarm` is instead a step CI can cache, and it reports what it skipped rather than showing a
spinner: a warm cache prints `0 pulled, 5 already present`, and a run that pulls when it should
not is the cache being broken. That line is the only thing worth reading in the step's log.

Docker only, via a `Puller` capability. In a cluster there is no local image store to warm — the
image has to be on whichever node the scheduler later picks, which means a DaemonSet sbx would be
creating in somebody's cluster uninvited.

[daytona-builder]: https://www.daytona.io/docs/en/declarative-builder/


---

## Scope, isolation and trust

### sbx is a tool people run, not a service anyone offers

The obvious next step from a control plane is multi-tenancy: authentication, per-user isolation,
quotas, and eventually somebody hosting it. That is not the direction.

sbx exists to be adopted into other people's workflows — a binary they run on hardware they
already have, for their own branches, agents and CI. It is not trying to become the thing you buy
instead of E2B, and the comparison tables should be read that way: which tool fits a job, not
which vendor wins.

Three consequences, so this is a decision rather than a mood:

**No auth, no tenancy, no quota**, and the README says so where someone might deploy it anyway. A
shared box for a team that trusts each other is the supported shape.

**The GoFr console is for the operator, not for tenants.** Metrics, health and a read-only view
of what the daemon is doing. It is not the seam through which sbx becomes hosted, and the API
stays read-only for that reason as much as for the lifecycle one.

**A tunnel to your own deployment is not tenancy.** `sbx serve --connect-addr` and `sbx connect`
let one person reach a sandbox they deployed, from a laptop that cannot run it — a data-plane
tunnel with one shared token proving you own the deployment, the same posture as an SSH key on
your own dev box, off unless the flag is passed. The three consequences still hold: no users, no
roles, no quotas, and the control plane is not on it. `create`, `rm` and `exec` remain
local-only, which keeps "one person's box, reached from their laptop" different from "a service
other people log in to".

The line, so a later change can be measured against it: the moment this grows a second identity —
per-user tokens, roles, anything answering "who are you" rather than "is this yours" — it has
become the thing this section rules out, and the answer is a gateway in front rather than an
identity system inside.

**Amended for the OpenSandbox API (v0.9.0).** `sbx serve --osb-addr` puts `create`, `rm` and
`exec` behind an HTTP API, which the paragraph above said would not happen. It happened because
the point of the API is that clients written for OpenSandbox — its five SDKs, its CLI, its MCP
server, and the agents built on them — run against a machine you already have. What did not
change is the line itself: the API takes **one operator key**, the same "is this yours" posture as
the connect token, compared in constant time; there are no per-user keys, no tenants and no
quotas, and upstream's key-to-namespace multi-tenancy is deliberately not implemented. A team that
wants identities puts a gateway in front, exactly as this section already says.

*Corrected in v0.9.1.* v0.9.0 bound loopback by default **with no key there**, on the premise that
being on this machine answers "is this yours". It does not on a VM-backed engine - see "Loopback
is not a trust boundary on a VM-backed engine" below - so the key is now required on loopback too
(generated when not given), and a non-loopback `--osb-addr` is refused outright until
server-proxy mode exists.

### Loopback is not a trust boundary on a VM-backed engine

colima and Docker Desktop run containers inside a VM whose gateway forwards to the host's
`127.0.0.1`. Measured on colima: a host `nc -l 127.0.0.1 18999` answered a `docker run alpine
wget` at `host.lima.internal`, `host.docker.internal` and `192.168.5.2` alike. Docker Desktop
does the same through `host.docker.internal`. So "bound to loopback" keeps other *machines* out
and keeps nothing on the engine out - including sandboxes, which exist to run code nobody vetted.

v0.9.0 treated loopback as private and served the OpenSandbox API there with no key. Any container
could list sandboxes, read each one's execd token from its endpoint and run commands in any of
them. What follows from taking the reach seriously:

- **A key is always required.** Given with `--osb-key`/`SBX_OSB_KEY`, or generated once into
  `~/.sbx/osb/key` (0600, directory 0700) and reused; the log says where, never what. `sbx mcp`
  reads that file, for a loopback `--url` only. Keyless is `--osb-insecure-no-key`: typed, never
  defaulted, loopback only, and warned about on every start.
- **Nothing a sandbox holds may authorise anything beyond that sandbox's own execd.** execd's
  token must be in the container, so the egress sidecar route - which changes the sandbox's own
  filter - has a separate credential, stored only in the API's 0600 record and handed out only
  to a caller that already has the key.
- **Non-loopback is refused, not keyed.** The endpoints the API returns are 127.0.0.1 listeners,
  useless to a remote client; serving them needs server-proxy mode, which is not built. Until it
  is, reach a remote machine's API through `ssh -L`, and its sandboxes through `sbx connect`.
- **An API sandbox has exactly one daemon.** Containers the API creates carry `sbx.osb`, and an
  unscoped daemon that does not serve the API leaves them alone, so the machine's own daemon
  cannot thaw a sandbox the API paused.

**Rejected: telling containers apart by source address.** On colima all three of those routes
arrive at the host listener from `127.0.0.1` (measured: the peer address was `127.0.0.1:<port>`
for each), so the API cannot see who is calling - the premise that failed in the first place.

**Rejected: a unix socket instead of TCP.** It would keep containers out, but every OpenSandbox SDK
speaks HTTP to a host and port; an API they cannot reach is not the compatibility this exists for.

**"Hosted Postgres, operated for you" stays in the use-something-else table permanently.** Neon
is the answer there and always will be — not because sbx cannot branch and scale to zero, but
because "somebody else runs it" is the whole product, and this one is run by you.

### Isolation fails closed, and says why

Asking for a runtime the machine lacks never silently downgrades you. Docker refuses
immediately. Kubernetes' own default is to refuse silently, taking two minutes to report the
service "never became ready" when the real problem is a missing RuntimeClass — so sbx checks
first and says so in one second, rather than letting that report stand in for a diagnosis.

### A sandbox has one isolation tier

`--isolation` is chosen when a sandbox is created, and nothing recorded it. `sbx add` takes the
same flag with the same default, `container`, so adding a service to a gVisor sandbox ran it on
runc beside services under runsc. Nothing warned, and `sbx list` showed nothing different. The
sandbox kept its name and lost the property it was created for. Re-creating a service whose
image changed had the same hole: a bare `sbx create` passes `container` too.

Every docker container now carries its tier as the `sbx.isolation` label, and the sandbox's tier
is the one its services were created with:

- `sbx add` with no `--isolation` (and no `SBX_ISOLATION`) joins the sandbox's tier.
- An explicit tier that differs is refused, naming both tiers and the recreate that gets the
  other one. The flag and the variable both count as asking.
- A service re-created because its image changed keeps its container's tier, not the command's.
- A container without the label predates it and was created as `container`, so it reads as that.
  A provider that records no tier (firecracker, kubernetes) is not checked.

**Rejected: warn and carry on.** A warning is a line in a scrollback. The sandbox left behind is
still mixed, and the next reader of `sbx list` has no way to find the service that is not.

**Rejected: let the flag win per service.** It is expressible, but the services of a sandbox talk
to each other on one bridge. One runc service there is the weakest member, and it is the
sandbox's isolation, not the service's, that a user chose.

**Rejected: read the runtime back from docker (`HostConfig.Runtime`).** It says `runsc` or
`kata-runtime` on this engine, but the name is whatever the operator registered. Mapping it back
to a tier would be a guess, and a label is how sbx already remembers everything else about a
container.

### Capabilities are negotiated, not stubbed - and sbx does not reach around a provider

The obvious way to add snapshot support is four new methods on the core `Provider` interface —
and that is not an interface: a method on `Provider` is a promise every backend keeps, and
kubernetes can only keep it by implementing all four as stubs that return errors, which is a
docker client with a kubernetes-shaped hole in it.

They are an optional `Snapshotter` interface instead: a provider implements it if it can do the
thing natively, and the CLI asks with a type assertion and reports one refusal naming the
backend. It is the negotiation `--isolation` already uses — declare what you want, be told
plainly when this backend cannot give it.

The naming rule that follows: **a capability is named for what the user wants, never for how a
backend does it.** `Snapshotter`, not `Committer` — the kubernetes answer is a volume snapshot
through its own CSI, not `docker commit`, and an interface named after docker's verb would make
the correct implementation look like a workaround.

**And sbx does not reach around a provider to do something the provider cannot.** One option for
egress control was having sbx launch a privileged container to write `DOCKER-USER` iptables
rules on the host. It would have worked on a given laptop — but it is sbx mutating a host
firewall from outside the abstraction it claims to have: invasive, unverifiable where it cannot
be tested, and correct only while docker happens to be arranged one particular way.

The provider-neutral shape is a spec field saying *what* is wanted — deny egress — with each
backend implementing it natively or refusing: NetworkPolicy in a cluster, and for docker a
primitive that does not currently exist, since `--internal` and `--network none` both stop port
publishing and make a sandbox that can never be woken.

### Tunnels are shelled out, and the anonymous one is opt-in

**Status:** current for `sbx url`. `sbx connect` (v0.3.0) is sbx's own WebSocket tunnel, but only between sbx and sbx; it does not replace a public-URL tunnel.

Cloudflare reached the same conclusion about their own SDK in 2026, replacing `exposePort()`
with Cloudflare Tunnel.

Falling through to an anonymous third party automatically the moment ngrok fails is the wrong
default — failing toward *less* trust should never happen silently. So `--via ssh` must be typed
explicitly. It uses `StrictHostKeyChecking=yes` rather than `accept-new`, and admits in its own
note that the operator publishes no fingerprint to pin against.


---

## Snapshots and checkpoints

### A snapshot is the volume, not the container

`docker commit` is the obvious way to save a sandbox, and it does not work: it does not capture
mounted volumes. A seeded postgres committed this way produces an image whose data directory
holds **zero files** against the live container's twenty-four — the fork's server starts, its
database is empty, the worst kind of failure because it looks like it worked.

Everything worth snapshotting in sbx lives in a volume; `volume` is the field that makes
sleeping safe, so by construction that is where state is.

So a snapshot copies volume to volume through a throwaway container — docker's own recipe: it
stays in docker's storage, needs no host path (colima would not share one anyway), preserves
ownership (postgres requires it before starting on a data directory at all), and never streams
bytes through the sbx process. The image is still committed, for services that keep state
outside a volume.

Restore happens **after** create and **with the service stopped**. Create starts each service to
health-check it, so a database has already initialised an empty data directory by then, and
writing over that while it runs replaces the floor underneath it. `init` is dropped from a forked
spec for the same reason: it has already run in the state being forked, and running it again
re-seeds a seeded database.

The fork keeps its own `volume` declaration, rather than assuming the image carries the data —
that assumption is exactly the one `docker commit` gets wrong.

### Memory checkpoint goes through podman, because docker's restore path doesn't work

**Status:** current for container providers. A microVM restores memory natively, as its sleep; see "A firecracker snapshot restores only as itself".

`sbx checkpoint` / `resume` save a running process with CRIU, not just its disk. The obvious
primitive is `docker checkpoint create` / `docker start --checkpoint`, and it only half-delivers:
the **dump** is fine, but the **restore** fails. Measured on a Linux host (Ubuntu 24.04, kernel
6.8, built CRIU 3.19, docker 29 with `--experimental`): `docker start --checkpoint` dies with
`bind-mount /proc/0/ns/net -> …: no such file or directory`, and with `--network none` it dies
with `content … already exists` instead. Two different docker/containerd bugs; the feature is
effectively unmaintained upstream.

CRIU itself is not the problem. `criu check` passes on the same kernel, and **podman** — whose
CRIU integration Red Hat maintains for production use — checkpoints and restores the identical
workload cleanly: a redis started `--save "" --appendonly no` (no disk persistence at all), with a
key set only in memory, comes back after `podman container checkpoint` + `restore` with the key
**present**. The only way that key survives is a real memory restore.

So the provider detects podman (by its socket, confirmed by the version components) and drives
`podman container checkpoint` / `restore` in place, and keeps the docker path only as the
fallback it is. The whole cycle is proven end to end through `sbx checkpoint` / `sbx resume`
against a podman runtime; on docker the restore is docker's to fix. This is why `sbx doctor` and
the help describe checkpoint as reliable on a **podman** runtime, not merely "Linux with
experimental docker".


---

## Egress

### Egress is denied by a bridge without NAT, not by a firewall sbx writes

**Status:** current for `egress: "deny"`. The gap it names under "What it cannot do" was closed by the egress filter: `egress_allow` (v0.7.0), `egress_policy` (v0.9.0).

`egress: "deny"` puts the service on a per-sandbox bridge created with
`com.docker.network.bridge.enable_ip_masquerade=false`. No masquerade means no NAT off the host,
so nothing routed leaves — and docker still publishes ports into that bridge, so the wake path is
untouched. Measured: published port answered 200, an outbound fetch was blocked, and the service
still slept and woke on a connection.

Two alternatives were considered and rejected:

**`--internal` and `--network none`** block egress and also stop docker publishing the port at
all, so the sandbox can never be woken. A security control that breaks the thing it protects will
be turned on by someone who then trusts it.

**iptables rules in the `DOCKER-USER` chain**, applied by sbx launching a privileged container.
This works on the machine it is written on, and it is also sbx reaching around its abstraction to
mutate a host firewall — invasive, untestable where it cannot run, correct only while docker is
arranged one particular way. The bridge asks docker to do it, the difference between configuring
a backend and operating on the host behind its back.

The kubernetes provider refuses the field rather than ignoring it. Its answer is a NetworkPolicy,
only some CNIs enforce them, and a control that silently did nothing is worse than one that says
no.

**What it cannot do.** Every rival allows and denies **by domain, CIDR and IP** — E2B by
wildcard, Daytona as a firewall. This is all-or-nothing, and closing that gap needs something
that terminates or inspects connections:

- a **filtering proxy** the sandbox is pointed at, which means TLS termination or SNI inspection,
  a certificate the workload trusts, and a process that is not 0 B at rest;
- or **rules in the `DOCKER-USER` chain** matched to the container's address, which works on
  Linux and must run inside the VM on macOS — a capability that would degrade with a reason where
  it is absent, exactly like `--isolation gvisor|kata`.

This section used to end: *"Neither is a flag today, which is why the first attempt at one was
reverted rather than shipped."* It was true when written and stopped being true when the filtering
proxy was built - `egress_allow`, a domain allow-list, and then
`egress_policy`, OpenSandbox's network policy with domain, wildcard, IP and CIDR rules that change
on the running service. It took neither a trusted certificate nor TLS termination, because a
CONNECT names its host and plain HTTP names it in the request - the proxy decides on that, and
splices the TLS it never opens. And it took no `DOCKER-USER` rule, because this bridge already
has no route out: the proxy does not have to *stop* traffic that walks around it, since none can.
What shipped is a component with a lifecycle, which is what this section said it would have to be. `egress: "deny"`
itself is still coarse, and still the right answer for a box that needs nothing at all.

### Traffic through the egress filter counts as activity, stamped on bytes

sbx sleeps a service on the bytes through its port, which is the right measure for a database, a
cache, or a dev server: something dials them, and if nothing has for five minutes, nothing needs
them. It is exactly wrong for the box an agent works inside. Nothing dials that. The agent reads
files, edits them, compiles, and calls a model API — and every one of those is invisible to a
proxy that only sees inbound traffic. Such a box looked idle from the moment it started working,
and the only setting that kept it alive was `idle: "never"`, which holds its memory for as long
as the sandbox exists. That is the cost sbx exists to avoid, reintroduced by the feature meant to
make agent boxes usable.

One of those four things is not invisible. A box with `egress_allow` reaches the outside world
through a filtering proxy sbx already runs, in sbx's own process, in the data path, on every
request. So the call out is counted: **the filter stamps the units behind its gateway as active.**
No new mechanism, no protocol to understand, no agreement with the workload — a box that is
working stays awake, and one whose agent has stopped sleeps on the ordinary timer.

Two details decided the shape:

**Stamped on bytes, not on connections.** A CONNECT to a model API stays open for as long as
tokens are streaming back, which is minutes. A stamp when the tunnel opened would let the idle
window close in the middle of one. So the stamp sits in the copy loop, which is the same rule the
inbound side is measured by — "bytes, not connections" — applied to the way out. Cost, measured:
**+2.1 ns per 32 KiB chunk and zero allocations** against the same copy with no hook.

**Throttled to one stamp a second per gateway.** Stamping walks the unit map under the daemon
lock, the same lock the wake path takes, and a large download would otherwise take it once per
chunk. Idle windows are minutes; a second is finer granularity than the decision can use. The CAS
in `due` is what makes two concurrent streams cost one walk rather than two.

**A refused request stamps nothing.** Otherwise a box hammering a host it may not reach could
hold its own memory open forever without ever getting anywhere — the failure mode is a loop that
never sleeps, so admission has to be on the permitted side of the check.

What it does not cover: a box with no allow-list, and outbound traffic that is not HTTP. There
sbx still sees nothing and `idle: "never"` is still the answer.

### A live egress policy is held by the filter and pushed to it

OpenSandbox changes a sandbox's network policy while it runs, and so does `sbx egress`. The filter
already existed in two forms - a listener inside the daemon where the daemon can bind the bridge
gateway, and a container on the bridge where it cannot (every Mac) - and the question was where the
policy lives and how a change reaches it.

**The filter holds it, in memory, and swaps it atomically.** A request is judged by one policy or
the other, never a mixture, and nothing restarts: restarting the filter would cut every tunnel open
through it, and "change it without recreating anything" is the whole feature. A hosted filter is
swapped by the daemon directly. A container filter is told over **the loopback port it already
publishes for activity scraping** - a token-guarded `GET`/`PUT /policy` on the same listener as
`/last`. It is the one port of that container the daemon can already reach, on every docker sbx
supports, so the control channel needed no new port, no socket mount and no second listener.

The token is not decoration. That listener is on every interface the container has, including the
sandbox's own bridge, so without it the workload could rewrite its own policy. It is generated per
filter container and carried as a label, which is readable by whoever can `docker inspect` - who
can already do anything to the container.

The OpenSandbox API's own door to the same policy - the sidecar-shaped route behind
`endpoints/18080` - follows the same rule with a credential of its own. v0.9.0 accepted execd's
token there, and execd's token is in the sandbox's environment by necessity, so the workload could
rewrite its own policy. Since v0.9.1 each sandbox has a separate egress credential, kept only in the
API's 0600 record and handed out only to a caller holding the API key.

**Only replace crosses the wire.** Merge, remove and reset are computed by the caller from a `GET`,
and the `PUT` carries `If-Match` with the hash it read. Two writers - the CLI and the OpenSandbox API
- cannot silently drop each other's rules; the loser re-reads and retries.

**Persisted twice, for two different failures.** The container writes what it was told to its own
disk, so a reboot restarts it enforcing the live policy rather than the one on its command line - the
looser one, typically, since boxes are usually locked down after they start. And the host keeps a
copy in `~/.sbx/egress/`, which the daemon pushes back to a filter container that was replaced, and
which a hosted filter reads on start and within a second of a CLI change. The host copy records the
hash of the declaration it was made against, and `sbx rm` removes it, so a sandbox recreated under
the same name does not inherit exceptions to a policy it no longer has.

**Rejected: rebuild the container with the new policy on its command line.** It is what the
allow-list did when it changed, and it drops every open connection, takes seconds, and turns a
lock-down into a window where the box has no egress at all.

**Rejected: mount the policy file into the filter and have it watch.** It needs the host path to be
visible where docker runs: on a remote `DOCKER_HOST` it is on another machine, and on a VM-backed
docker it depends on how that VM shares files - the same seam `files:` already has to diagnose. The
port is reachable wherever the activity scrape already works.

**Rejected: per-service policies inside one sandbox.** The filter is per bridge and a `CONNECT`
carries nothing that says which container opened it. The source address could, but it changes every
time a service sleeps and wakes. A write naming one of several services that share a filter is
refused, and the answer is a sandbox each - which is what an OpenSandbox sandbox already is.

### Default-allow is enforced by the same door, and it costs raw TCP

OpenSandbox's policies are often `defaultAction: allow` with a few denies - no metadata endpoint, no
internal ranges. On a bridge with NAT that would be advisory: a client that ignores `HTTP_PROXY`
dials the denied range directly. So a default-allow service goes on the same no-NAT bridge as a
deny-default one, and every deny is real because nothing leaves except through the filter.
Measured: under `deny 1.1.1.0/24`, a direct dial from inside the box to `1.1.1.1` and to `8.8.8.8`
both had no route, `1.1.1.1` through the proxy got 403, and with masquerade switched on for the same
test the direct dial got through - the failure the arrangement exists to prevent.

The price is that *open* means open to what a proxy carries: HTTP and HTTPS. Raw TCP to the
internet has no way out. That was the other option's price too, in reverse - an open bridge with a
proxy nobody is forced through is a policy that holds only for clients that agree to it, and a
deny rule that holds only for polite clients is the control that looks like one and is not.
Refused rather than pretended, as `egress_allow` on a machine that could not run the filter was.

Two consequences of where the filter sits. It resolves hostnames itself, checks every address
against the address rules and dials the one it checked, so a permitted name cannot be rebound into a
denied range. And it refuses its own loopback and link-local unless a rule names them: upstream
enforces inside the sandbox's namespace, where `127.0.0.1` is the sandbox; sbx's filter is on the
host or beside the box, where `127.0.0.1` is somebody's docker socket.

### The container filter refuses the machines behind it, and carries two ports

**Status:** current, 2026-09-29. Corrects the entry above in two places: "no raw TCP" was not
enforced, and the container filter's loopback was not the only thing behind it.

Measured on colima before this change, from inside a sandbox under `egress: "allow"`:
`CONNECT host.lima.internal:<port>` reached a listener bound only to the Mac's `127.0.0.1`, as did
`192.168.5.2` and `host.docker.internal`; `CONNECT 172.17.0.1:22` answered with the VM's
`SSH-2.0-OpenSSH_9.6p1`; `CONNECT 1.1.1.1:53` answered 200. The workload has no route to any of
them. The filter gave it one.

**Ports: 80 and 443, plus what `egress_allow` names.** A proxy that tunnels a `CONNECT` to any port
is a TCP relay, and the promise was HTTP and HTTPS. An `egress_allow` entry written as `host:port`
always parsed and its port was dropped; it is now the one way to add a port, for that host only,
matched on the name the client wrote so a name's grant does not open its addresses to other names.
`egress_policy` stays OpenSandbox's shape, which has no port, so it gets 80 and 443.

**The machines behind a container filter are refused like the host is behind a hosted one.** The
filter gets a `Refuse` set, which no rule opens: every docker network's gateway and the default
bridge's subnet (named by the provider at start), its own routes' gateways, and the `/24` around
what `host.docker.internal`, `host.lima.internal` and `gateway.docker.internal` resolve to. The
`/24` rather than the address because it is the VM's link to the host: on colima `.2` is the Mac,
`.3` its DNS and `.15` the VM.

**Rejected: refuse every private range, as the microVM filter does.** It would break a
default-allow container sandbox that reaches a LAN or VPN host through the proxy, which SPEC.md
allows, and would need the operator switch the microVM filter has for widening it. The
engine's own addresses are what the report showed, and they are closed.

**Rejected: a network of the filter's own for its route out.** It would keep the filter off the
default bridge and its neighbours, at the cost of a second bridge per filtered sandbox, and
docker's address pools hold about thirty. The default bridge's subnet is refused instead.

**Rejected: refusing `--isolation gvisor` with `egress_allow`.** gVisor does not use docker's DNS
on a user-defined network, so the filter's alias never resolved. The filter now sits at the last
address of the sandbox's subnet (docker allocates from the bottom, and `--ip` reserves it), and
each service gets it through `--add-host`. A fixed address survives the filter being replaced, so
the hosts entry, written once, stays true.

What it does not cover: a docker network created after the filter started. Its gateway is on the
VM and is not in the list until the filter is recreated; with ports limited to 80 and 443 that is
the VM's own web ports on that address, if it serves any.


---

## OpenSandbox API

### An API snapshot is the container, because that is where its state is

The rule above - a snapshot is the volume - follows from where a spec sandbox keeps its state:
`volume` is the field that makes sleeping safe, so that is where the data is. A sandbox created
through the OpenSandbox API has no such volume. Its image is somebody's `python:3.11`, and
everything the caller did - `pip install`, a cloned repo, a file an agent wrote - is in the
container's writable layer. Copying a volume would snapshot nothing.

So `POST /sandboxes/{id}/snapshots` is `docker commit` of the sandbox's one container, to
`sbx-osb-snap:<snapshot id>`. It is the same rule, applied to a different place: snapshot what
holds the state. Upstream's docker runtime does the same (a commit to its own repository, tagged
with the id), which is the reference for behaviour here.

- **Consistency is docker's pause.** `docker commit` pauses the container for the copy and
  resumes it after, which is the "may temporarily pause the sandbox" the spec allows. sbx does not
  add its own freeze around it: the daemon holds idle freezes and API pauses as state of its own,
  and a second pauser would have to agree with it about who thaws. A sandbox frozen by the idle
  policy is committed frozen and stays frozen.
- **What is not in it, said plainly.** Memory and processes (a fork starts cold, as `sbx fork`
  does), and every `host` or `pvc` volume - those are the caller's storage, not the sandbox's, and
  upstream leaves them out too. The execd volume at `/opt/sbx` is a mount, so it is not in the
  image either, and a fork mounts its own.
- **A fork is a new sandbox.** New id, new execd token, the snapshot's image with no pull (it
  exists on this engine only), and `tail -f /dev/null` when no entrypoint is given - the spec's
  default, and necessary: a committed image's ENTRYPOINT is the execd wrapper the source ran
  under. The committed image does carry the source's environment, token included; the fork's own
  token overrides it, and the image never leaves the engine that made it.
- **Its lifetime is the API's.** Not `sbx-snap-*`, which `sbx gc --snapshots` sweeps as the CLI's:
  an API snapshot is meant to outlive its sandbox until someone `DELETE`s it. A delete is refused
  while a sandbox or a template still runs on it, rather than forcing `docker rmi` and leaving a
  sandbox on an untagged image nobody's record names.

A template, in the same spirit, is what upstream's templates are *for* - a fixed workload to start
many sandboxes from by id - without the part sbx does not have (a microVM image published to S3):
an image or a snapshot id, plus the entrypoint and limits. Fields that only mean something to that
build (`readiness`, a `disk` size) are refused rather than stored and ignored.

### Volumes on the API: host paths are the operator's to allow, and claims are namespaced

`host` volumes are a sandbox writing to this machine's disk, so none are allowed until the operator
lists roots with `sbx serve --osb-host-paths`. A path is checked as written and again after
following symlinks - a link inside an allowed root that points at `/` passes a string check and
escapes at mount time - and the directory is created as the user running sbx, not by docker as
root. It is created only once the whole request has passed, and resolved and checked again
immediately before the container is created, so a directory swapped for a symlink in between is
refused. (Until v0.13 that last check sat only on the allocate-a-slot create path, and the docker
provider picks its own slot, so on docker it never ran.)

Docker resolves a bind source again at **every container start**, so a sandbox that slept
(`sbx.idle=sleep`, `sbx sleep`) and wakes would follow a link swapped in while it was down. Since
v0.13 the canonical paths the API validated are pinned on the container (label `sbx.host-binds`),
and every start - the daemon's wake, `sbx wake`, a checkpoint restore - first re-asks the create
question: each still resolves to exactly itself and is a directory, or the start is refused and
docker is never asked. The alternative, re-creating the container from the resolved path at each
wake, was rejected: it loses the container's writable layer, and it re-resolves the path anyway,
so it narrows the window no further. Refusing is the fail-closed choice - a moved or replaced
directory stops the sandbox with a message naming it, rather than mounting something else. The
window left is the one create also has: between this check and docker's own resolution. A frozen
sandbox (the default idle) never restarts its container and is not affected either way. The roots
remain the operator's, and a root a sandbox's adversary can write to is still not one to list.

They are mounted with `--mount`, not `-v`: `-v` creates a missing bind source, and on a Mac
it creates it inside the runtime's VM, where the caller's files are not. Refused is the useful
failure.

`pvc` is a docker named volume, as upstream's docker runtime makes it - but named
`sbx-osb-pvc-<claimName>`, where upstream uses the claim name verbatim. Verbatim would let any API
caller mount any volume on the engine by naming it: another sandbox's database, the execd volume,
something that has nothing to do with sbx. `deleteOnSandboxTermination` removes only a volume that
create made, never one that already existed, and `sbx gc` never treats these as orphans - they are
a caller's storage with no sandbox to be orphaned from. A cluster answers 501 until a
PersistentVolumeClaim, whose storage class and size are the operator's decisions, is built.

### Warm-pool members wait pinned running, and every claim re-keys

A pool member was first frozen while it waited. Every claim then paid a `docker unpause`, which
dockerd serialises: twenty at once measured 200-430 ms on colima, a burst of 20 claims ~870 ms.
Members now wait running and pinned (the reaper leaves them alone until the claim); a waiting
member costs `tail` and an idle execd. `--osb-pool-freeze` restores freezing.

Every claim re-keys execd with a token minted for the request, env or not. A member's token
existed before its caller did, and a secret minted ahead of the caller can be shared - a clone
of a snapshot carries it. The round trip is the price (docs/BENCHMARKS.md). What a member may
serve is a whitelist of request fields; anything else, including a field this sbx does not know,
takes the cold path rather than being dropped by a member made without it.

A create that takes the cold path while pools exist is logged at info, naming the field that
differed from the nearest pool (or the field no member can carry), once per distinct miss per
ten minutes. Silent, it cost a test run its warm path unnoticed: curl creates omitting
`resourceLimits` never matched members built with the SDKs' cpu 1 / memory 2Gi. Once per miss
rather than per create, because a client that misses does so on every create, and a line each
time would bury the log it is meant to explain. Not a warning: going cold is correct, only slower.

### An API sandbox's health check runs once a minute, and quickly only while it starts

Every docker health check is a runc exec inside the container. At the 5s interval API sandboxes
used to declare, that is 0.2 execs a second per sandbox, forever - 20 a second at 100 sandboxes -
for an answer almost nothing waits on: the wake path runs the same command itself (`Probe`) rather
than waiting for docker's verdict, and a create reports Running on execd's own `/ping` through the
wake port. What does read docker's status is the idle clock, which will not start until a unit has
been seen healthy once, so that first report has to arrive promptly after a start and nothing after
it has to be fresh.

So the check runs every 60s, with `--health-start-interval 1s` inside the 60s start period. Docker
uses the start interval only until the first healthy result, so a sandbox pays one exec about a
second after each start and then one a minute. The flag needs Engine API 1.44 (Docker 25); sbx asks
the engine's version once and leaves it off on an older one, which then reports healthy on docker's
own schedule inside the start period rather than refusing the create.

Measured on the osb colima engine (Docker 29.2.1, API 1.53), 10 containers per arm running
together, `docker events` counting `exec_start`:

| | first 60s | next 60s | first check after start |
|---|---|---|---|
| before: `--health-interval 5s` | 116 execs (1.93/s) | 118 (1.96/s) | ~5 s |
| after: `60s` + start interval `1s` | 10 (0.16/s) | 10 (0.16/s) | 1.4 s |

12x fewer execs, and the first healthy report sooner rather than later.

**Rejected: drop the docker health check once execd has answered through the wake port.** A
container's health config is fixed at create, so dropping it means recreating the container - or
never declaring one, and then the wake path has nothing to run and falls back to sleeping two
seconds and hoping, which is exactly what declaring it avoided.

### What the API remembers lives in its record file, not in labels

Metadata, expiry, the execd token, the held-pause flag and the pvc volumes a sandbox owns are in
`~/.sbx/osb/<id>.json` (0600) and nowhere else. The design said metadata would also be written as
labels; it is not, because docker labels are fixed when a container is created. A label copy is
stale after the first `PATCH`, and relabelling means recreating the container - a restart, with its
processes and memory gone, that the caller did not ask for. Labels carry only what docker's own
view needs and nothing the API edits: sandbox, service, slot, ports, idle policy.

The cost is that a record file lost is metadata lost while the container survives. That is the
same trade the rest of this state makes (expiry is in the same file), and the alternative is two
copies that disagree after the first edit.

### An API sandbox freezes when idle; a sandbox.json sandbox stops

sbx's default is to stop an idle container: 0 B held, woken by `docker start` in about 110 ms, and
anything that was running in it is gone. OpenSandbox's contract is the opposite - a background
command started in one request is expected to be running at the next, however long the gap - and
stopping breaks it silently: the next request succeeds against a sandbox whose server is no longer
there. So an API sandbox's default `on_idle` is `freeze` (`docker pause`, the cgroup freezer):
memory and processes kept, no CPU, thawed in about 10 ms by the next byte.

It is the API's default and not sbx's because it holds memory, and holding nothing is why sbx
exists. `extensions["sbx.idle"]="sleep"` opts an API sandbox back into stopping; `on_idle:
"freeze"` opts a `sandbox.json` service into freezing. A frozen sandbox the caller paused is a
different thing - held, reported as `Paused`, and not thawed by traffic (ARCHITECTURE.md).

### The OpenSandbox API is not on a cluster, for now

(Until v0.12 this entry was "docker-only". A microVM runs the agent itself and serves the API on
Linux - the next entry. A cluster still does not.)

Every API sandbox runs sbx's agent, execd, inside an image the caller chose and sbx did not build.
On docker that is a named volume seeded once and mounted read-only at `/opt/sbx` - the `Injector`
capability. A cluster's equivalent is an init container copying the binary from an image the nodes
can pull, which is a different mechanism with a different trust story (whose registry, which
digest), and it has not been built. So `POST /v1/sandboxes` on the kubernetes provider answers 501
naming the missing capability, rather than creating something that cannot become ready.

`pause` would be refused there regardless. Scaling to zero keeps the filesystem and discards the
memory, and OpenSandbox's pause is a promise that a process running before it is running after it.
The original design's "k8s: scale to 0, reported honestly in `status.message`" was a pause that
does not pause with a note saying so - the stub the capability pattern exists to avoid.

**Rejected: a kubernetes path that bakes execd into a derived image.** It would mean sbx building
and pushing images to the operator's registry on every create, for every image anyone names.

### The OpenSandbox API on a microVM: the agent is PID 1, the token is the API's, a snapshot is the disk

**Status:** current, with amendments marked inline: Running means execd answers (after v0.13.0), and the helper-VM path is served (v0.13.0).

v0.12 serves every API route on `--provider firecracker` (Linux with `/dev/kvm`), because the
point of a microVM - untrusted code that must not share the host kernel - is exactly what a public
API sandbox is. Each difference from the container path is a decision, not an accident:

- **`RunsAgent`, not `Injector`.** A VM's PID 1 is `sbx fc-init`, which already becomes execd with
  the workload as its child (the agent rides on its own drive - "The image rootfs holds the image").
  So the provider declares the capability *this sandbox answers execd without being given it*, and
  the API skips the execd volume seed, the `/opt/sbx` mount, the entrypoint wrapper and the
  `/bin/sh` health command that runs the agent from that volume. Readiness is execd's own port
  accepting on the guest's TCP stack, which in a VM is the listener itself, not a proxy.
- **The token is the API's.** The API mints it and hands it to callers in endpoint headers; the
  provider boots execd with that `EXECD_ACCESS_TOKEN` (exactly once in the guest env) and re-keys a
  restored execd with it. It mints its own only for a `sandbox.json` service, which has no API to
  mint one. Two tokens for one sandbox is a sandbox that refuses the credentials the API handed out.
- **Born running.** A `sandbox.json` VM is created asleep (booted, snapshotted, killed). An API
  sandbox's contract is a process that runs, and the API reports Running only once execd answers,
  so a snapshot and restore at create would cost a second boot's worth of time to end where the VM
  already is. It is left running with `SnapshotValid=false`; its first sleep takes the Full
  snapshot, and if it dies awake its next wake cold-boots its disk.
- **Pause is a VM pause; idle is a VM pause.** `Pauser` is Firecracker's own pause (memory kept, no
  vCPU), so the API's pause and `on_idle: freeze` both mean what they mean on docker.
- **A snapshot is the disk, and a fork cold-boots a copy of it.** The plan allowed a memory restore
  as a new sandbox (per-VM drive copies, `network_overrides`, a mandatory re-key) if it proved safe.
  It is not safe yet, for reasons measured or read, not guessed: the guest's IP is set by the kernel
  at boot (`ip=` on the command line) and lives in its memory, so a clone in another slot comes up
  on a bridge whose subnet it does not have - every TCP port but execd's (vsock) unreachable, and
  egress on the wrong gateway; and a memory clone carries every secret userspace made before the
  snapshot (the spike measured execd's token identical in every clone at N=2..50 before re-key
  existed; re-key fixes execd's, nothing fixes the workload's). A disk fork has neither problem
  and is exactly what docker's API snapshot already is (`docker commit`: filesystem, no memory -
  "An API snapshot is the container"). So `Commit` of an API sandbox runs the agent's own `fssync`
  in the guest through execd (an image with no `sync` still has `/opt/sbx/sbx`), pauses the VM,
  copies (reflinks where it can) the root filesystem, and resumes it - or leaves it frozen if it was.
  The saved record keeps the image config (command, env, working directory) and no token or
  secret; a create from it is a new VM with its own agent drive, token, slot and address.
  A memory fork stays a follow-up, gated on the guest re-addressing itself on re-key.
- **`pvc` is an ext4 image on its own drive.** One sparse file per claim (`SBX_FC_VOLUME_SIZE`,
  default 10G, costing what is written), namespaced `sbx-osb-pvc-<claim>` as on docker, attached
  after the rootfs and mounted by fc-init at the mount path (a `subPath` bound over it,
  `readOnly` kept on both). Where docker lets two containers share a named volume, two kernels
  mounting one ext4 corrupt it: a volume is attached to **one VM at a time**, once per VM, and
  removing one that is attached is refused (a sleeping VM's snapshot names its path).
- **`host` volumes are refused by name** (`HostVolumes`, which docker has): Firecracker has no
  virtio-fs, and the refusal comes before the operator's allow-list, so it names the provider.
- **Running means usable, Jupyter included.** A cold boot plus Jupyter's own start is slower in a
  VM than in a container, and the first CI run reported a code-interpreter sandbox Running while
  its Jupyter was still starting: the SDK's first `/code` call then spent its whole deadline
  waiting inside execd and failed `context canceled`. The API's readiness probe is now
  `/ping?ready=code`: execd answers 503 with a sentence while an image-configured Jupyter
  (`JUPYTER_HOST`/`JUPYTER_PORT`) does not answer, and 200 at once for an image with none - so
  nothing else waits longer. The same on docker, where it is the same execd. Once execd answers, the
  wait for Jupyter has its own bound (3 minutes by default, `CodeReadyTimeout`), and a Jupyter that
  never comes up fails the create with "Jupyter did not answer within N (execd up after M ...)".
  Every create logs one line saying how long execd and then Jupyter took. Plain `/ping`
  is still liveness, which is what the wake proxy wants.
  *Reversed after v0.13.0: Running means execd answers, as upstream's server has it.* The wait
  above held a code-interpreter image whose entrypoint never starts Jupyter `Pending` until it
  failed, and upstream's own `e2e_test.go` does exactly that (`tail -f /dev/null`), so all
  three of its tests failed against sbx. Upstream reports Running once the container runs
  (`docker_service.py`, release-1.1.0) and leaves Jupyter to its SDK, whose
  `CreateCodeInterpreter` polls the kernel port itself (`waitRuntimeReady`). The failure that
  motivated the wait - a first `/code` call dying `context canceled` - was the vsock keep-alive
  bug (execd's connection returned `*fs.PathError`, and net/http cancelled every request after
  the first), fixed in v0.12.0, not a Jupyter that was slow to start. A client that skips the
  SDK's check still gets a bounded wait: execd's `/code` routes wait for a configured Jupyter
  that is starting (`JupyterStartupWait`). `CodeReadyTimeout` is gone; `/ping?ready=code` stays
  in execd for anyone who wants that answer.
- **A guest's console is bounded** (security review of v0.12, M3). `console.log` is the guest's
  serial console, appended to by firecracker for the VM's life, so a guest printing in a loop could
  fill the host's disk. It (and `vmm.log`) is cut back in place to its newest 4 MiB of whole lines
  once past 16 MiB - at every launch and on every daemon reconcile - with a line saying so; in place
  because firecracker holds it `O_APPEND`. Not a pipe through sbx: firecracker outlives the `sbx`
  process that started it, and a pipe reader would die with that process. Between two reconciles
  (the refresh interval, 15s by default) a guest can overshoot the cap by what its UART can write.
- **A frozen sandbox runs briefly while it is snapshotted.** A disk snapshot of a paused VM resumes
  it for the guest's `fssync` (bounded by the seal timeout), pauses it for the copy and leaves it
  paused - so its workload gets CPU for that moment. Copying an unsynced disk instead would lose
  whatever the guest had written but not flushed; a caller who froze a sandbox and then snapshots
  it gets a consistent disk at the price of those few hundred milliseconds of execution.
- **A disk snapshot keeps the image's env, not the create's.** The saved record is the image config
  (command, env, working directory) the VM was built from; the `env` a caller passed at create is
  not in it, so a sandbox created from the snapshot does not inherit it. Docker differs - `docker
  commit` bakes the container's env into the image. Passing the env again on the create from the
  snapshot is the workaround; carrying it in the record (less execd's secrets) is a follow-up.
- **Not yet:** the helper-VM path on a Mac or Windows (`--osb-addr` still refused there at
  startup). *Amended (K5):* no longer refused - the API runs in the helper VM's daemon and is
  fronted on the host; see "The OpenSandbox API through a helper VM runs in the VM". *Amended in v0.13:* the warm pool shipped on Linux direct - see "Warm-pool members wait
  asleep on a microVM". Egress on VM bridges shipped in v0.12 ("A
  microVM's only door is its filter").

### The OpenSandbox API through a helper VM runs in the VM

On an M3+ Mac or Windows, `sbx serve --provider firecracker --osb-addr` used to be refused. It now
starts the helper VM's daemon with the API on its loopback (22981) and the key in its root-only
environment file, and the host half reverse-proxies `--osb-addr` to it over the ssh forward the
connect endpoint already used.

- **Why in the VM, not a host-side API over `Remote`.** The VM is a Linux host with `/dev/kvm`:
  there the firecracker provider is `RunsAgent`, the jailer, the egress filter on each bridge and
  the host guard run natively, and every v0.13 refusal (no jailer without
  `--osb-insecure-no-jailer`, a guard that cannot be installed) is the same code as on Linux. A
  host-side API would drive VMs one `sbx fc call` at a time and need its own egress and guard.
- **The key is Linux's key, kept on the host.** `--osb-key`, `SBX_OSB_KEY`, else
  `~/.sbx/osb/key` generated on the Mac, where `sbx mcp` and the SDK examples read it. It is never
  on a command line in either machine.
- **Loopback is not a trust boundary, so the key is verified, not assumed.** The ssh forward is a
  port on the Mac's loopback, reachable from containers on a VM-backed engine whatever the host
  proxy checks; the in-VM daemon's key check is the one control for both. So the front refuses to
  serve with no key, and at startup proves the API behind the forward answers 401 without the key
  and 200 with it. `--osb-insecure-no-key` is refused on this path by name.
- **Endpoints listen before the create answers.** They are `127.0.0.1:<port>` in the VM and the
  mirror binds the same numbers on the Mac on a 2s tick; a create answered before the tick left the
  SDK's first dial refused. The proxy holds a successful `POST` under `/v1/sandboxes`, or an
  endpoint lookup, until the mirror has reconciled (`daemon.MirrorOptions.Sync`, bounded 5s).
- **Not yet:** the warm pool on this path (`--osb-pool` / `SBX_OSB_POOL` refused by name), and a
  live run on an M3+ Mac: this is proven by unit tests with a fake VM daemon and API only.


---

## MicroVM (firecracker)

### A microVM off Linux runs in a helper VM, not on Virtualization.framework

Firecracker needs Linux KVM. On an Apple M3+ Mac with macOS 15+, `--provider firecracker` runs
the linux build of the same sbx inside a Linux VM that sbx creates with nested virtualisation
(lima, else colima; `sbx-fc`, 2 CPU / 2 GiB by default), and on Windows 11 inside a WSL2 distro.
The spike measured it on an M4: a real `/dev/kvm`, Firecracker unmodified, 88 ms from snapshot
restore to first byte.

**Not Virtualization.framework, because it cannot do the one thing a microVM is for.** It
reports snapshot support and then fails to save one: the entitlement is Apple's own. A second
VMM there would cost cgo and the static binary, and still resume nothing (option C of the microVM plan in [ROADMAP at v0.14.0](https://github.com/aryanmehrotra/sbx/blob/v0.14.0/docs/ROADMAP.md#macos-pick-a-then-b-and-do-not-build-c)).

**The VM tool is shelled out**, for the reason tunnels are: lima and colima already solve the
VM, and `go.mod` stays empty. Every command names the instance, and the name must start with
`sbx-`, so nothing sbx runs can reach colima's `default` profile. colima's start can repoint the
*global* docker context - and the docker runtime is the one it needs, because the provider builds
each rootfs through a docker engine - so sbx records the context before every colima start and
puts it back after, even when the start failed. lima and WSL get docker installed inside instead.

**The rest of sbx is not reimplemented against the VM - it is run in it.** `create`, `list`,
`env`, `exec`, `logs`, `rm` and the rest execute unchanged in the VM, with the exit status
passed back, so every command the provider has works on the Mac the day it lands. The host half
of `sbx serve` follows the in-VM daemon's `/v1/fleet` and binds every sandbox port on the Mac's
loopback **at the same number** (`sbx connect`'s tunnel, re-asked on a tick so sandboxes created
later appear), so `sbx env` is correct on both sides and a TCP connect on the Mac is what wakes
the microVM. Only two in-VM ports reach the host - the connect endpoint and the OpenSandbox API -
over one ssh forward; lima's own port forwarding is switched off so a sandbox port is never
bound twice. WSL2 forwards loopback natively, so there it is not mirrored at all.

**Refused, with the fix, everywhere else**: an M1/M2 or an Intel Mac, macOS 14, Windows 10, WSL2
with `nestedVirtualization=false` (the refusal quotes the line to add), and a Linux host without
`/dev/kvm` (naming nested virtualisation, a metal instance type, or `--isolation gvisor|kata`).
It is a dev-parity path, not a speed one: at N≥5 concurrent restores nested virtualisation
collapses, so the macOS burst path stays the docker warm pool.

### One host probe decides the microVM path, and a Mac is sent to a helper VM, not refused

`sbx doctor`, `--provider firecracker` and the helper-VM layer (sbx inside a nested-virtualisation
Linux VM on macOS and Windows) all ask "can Firecracker run here, and how". They get one answer from
`internal/fc/hostcap`: a probe that only reads (the `KVM_GET_API_VERSION` ioctl on `/dev/kvm`,
DMI and cpuinfo for "this Linux is itself a guest", `sysctl` for the Mac's chip and macOS version,
`mkfs.ext4` on PATH) and a pure `Decide` returning **direct**, **helper-vm**, **kata-runtimeclass** or
**refused** with a reason and a next step. Two copies of that logic would disagree the day one learned
about a new chip, and doctor approving a machine the provider then refuses is the failure
"Isolation fails closed, and says why" exists to prevent.

The ROADMAP's option A was "refused on darwin". It became "helper-vm on a Mac that can nest", because
that answer is true and the refusal was not: an M3+ on macOS 15+ runs Firecracker unmodified inside a
Linux VM (spike, measured). The provider hands the decision to `HelperVMProvider`; a build without
that layer says it is missing rather than claiming the machine cannot. The Mac check parses the CPU
brand string instead of asking Virtualization.framework, because asking needs cgo, and the static
binary is half of what people install this for.

### A microVM's root filesystem comes from docker export, not from layers

The ROADMAP budgeted four weeks for pulling layers and applying them in userspace, honouring `.wh.`
whiteouts. `docker create` + `docker export` against the engine the host already has returns the
filesystem **already flattened** - the engine applied the whiteouts when it assembled the container
- with the engine's credentials, mirrors and platform selection. So there are no whiteouts to
honour, and a test fails if an engine ever exports one. A registry client in a zero-dependency module
would be a second copy of all of that.

The ext4 is built by `mkfs.ext4 -d`, shelled out like tunnels are. Owners are the trap: extracting a
tar as a non-root user makes every file yours, and postgres refuses a data directory it does not
own. e2fsprogs 1.47.1+ reads the tar directly and keeps them; older mkfs (Ubuntu 24.04 ships 1.47.0)
gets an extracted tree only when sbx is root, and otherwise the build is **refused** with both ways
out. The cache is keyed by the image ID, a content hash - a new image under an old tag rebuilds, an
unchanged one never does - per "A built image is keyed by its content".

Found against a real engine, not predicted: a `--format` template naming `.Config.Entrypoint`
fails on `alpine:3`, which has none. `Inspect` decodes the whole object instead.

### The image rootfs holds the image; the agent rides on its own drive

**Status:** current for the agent drive. The per-VM clone of the image it mentions was replaced by the layered root in v0.14.0; see the next entry.

Injecting `/opt/sbx/sbx` and an init into the image's ext4 would make every cached rootfs depend on
the sbx binary's hash, so rebuilding sbx - every dev build - would rebuild every image. Instead each
VM gets a small read-only **agent drive** (`vda`: `/sbx` and a 0600 `/init.json` with the entrypoint and
environment) that the kernel boots as root; `sbx fc-init` mounts the image (`vdb`), bind-mounts the
agent at `/opt/sbx/sbx`, switches root and becomes execd. It is an initramfs with a filesystem
instead of a cpio, so it costs no guest RAM. *Amended in v0.14:* the rootfs is no longer cloned per
VM - see the next section.

### A microVM links its image and writes to a layer of its own

Until v0.13 each VM cloned its image's root filesystem: `FICLONE` where the filesystem can (btrfs,
XFS), instant; its data extents where it cannot (ext4) - about 16 s for the 7.4 GiB code-interpreter
image on a CI runner, paid on every create of it. v0.14 copies nothing: a create links the image's
one cached `rootfs.ext4` into the VM's directory as `base.ext4` (a hard link: the state directory
holds the cache and the VMs), attaches it **read-only**, and gives the VM a writable drive of its own,
`upper.ext4` - a sparse, empty ext4 of `SBX_FC_DISK_SIZE` (10G by default). `sbx fc-init` mounts the
base read-only (`noload`: its journal is never replayed), the writable drive beside it, and lays
**overlayfs** over them (`upper/` and `work/` on the VM's drive) as the root it switches into - the
shape a container's root has under docker's overlay2. Both pinned kernels (6.18.48, x86_64 and
aarch64) have overlayfs built in; the sha256-pinned files were read for it, not assumed.

- **Shared like the kernel.** The base is one inode for every VM of the image, so it is staged into a
  jail exactly as the kernel is (`Stage.Shared`): linked only while it is root's and read-only, never
  given to the VM's uid, and forced back to that after the jailer has run. `fc.LinkShared` makes the
  cache file 0444 before it is linked (the builder wrote it 0600, which a jail would have been given
  a copy of - the copy this removes). A VMM that could write it would change every other VM's root.
- **The writable layer is the VM's**, staged as its own file, owned by its uid. Everything the guest
  writes to `/` lands there; its size is the most one VM can write to its root filesystem on the host
  (SECURITY.md).
- **Snapshots capture the layer, and link the base.** A sleep's memory snapshot names both drives;
  `sbx snapshot` of a `sandbox.json` VM links the base and clones the layer and the agent drive; an API
  sandbox's disk snapshot is the base (linked) and the layer (cloned), and a sandbox made from it
  links that base and starts from a clone of that layer. So a snapshot keeps its image alive by
  link count, whatever `sbx gc` does to the cache, and costs only what the source wrote.
- **Nothing moves under a VM made before.** The record says how its root is held (`layout`): a VM
  or snapshot from before v0.14 keeps its whole `rootfs.ext4`, read-write, for its life, and a sandbox made
  from such a snapshot copies it as then. `SBX_FC_ROOTFS=copy` makes new VMs that way too, for a
  guest kernel without overlayfs (`SBX_FC_KERNEL`); anything but `layered` or `copy` is refused.
- **Counted once.** `sbx doctor` counts each shared base once (by inode) as "shared images", and a
  VM's disk is its writable layer and agent drive.
- **The cost.** A write to a file that came from the image copies it up whole into the layer (as on
  docker), and the first boot of an image pays nothing more. Every create logs what its root
  filesystem cost (`microVM ... serving ... rootfs in ...`), and the microVM conformance step prints
  those lines, pass or fail.

### A snapshot is invalid from the moment its VM runs

A Firecracker snapshot is memory plus device state, and it is only correct against the disk as it
was when it was taken. Resume it and the guest writes; its page cache and its filesystem move on
together, and the snapshot now describes the past. If that VM dies without being slept - a host
reboot, a crash, a workload that exits and takes PID 1 with it - restoring the old memory over the
newer disk gives the guest a page cache that disagrees with its ext4, which is corruption, silently.

So `SnapshotValid` is written false, and fsync'd, **before** every resume, and true only after the
next snapshot completes. A Start that finds it false cold-boots against the disk as it is: a power
loss, which ext4's journal recovers from. A load that *fails* never ran the guest, so it restores the
flag. The same reasoning picks the snapshot type: a Diff is only correct on top of the memory file
this process was loaded from, so it is taken only then - never after a cold boot, and never after a
`sbx snapshot`, which resets Firecracker's dirty bitmap - and it is folded into the base by
`SEEK_DATA` extents, because a page the guest dirtied to all zeroes is data, and a non-zero scan would
restore what was under it.

### A sleep whose seal is not confirmed keeps the VM, re-keyed, and a guest cannot keep it for ever

**Status:** current; supersedes v0.11's stop-on-failed-seal.

v0.11 stopped a VM whose Seal failed or timed out: its next wake cold-booted, and whatever was only
in memory was gone. Two reasons, both still held: a VM left up after a Seal that DID apply answers
nobody until re-keyed, and a Start that finds it running only resumes it (up and permanently deaf);
and a guest that stalls its seal must not be able to hold its RAM. Its known issue - under host
memory pressure a Seal answer is late or lost (1 in 46 rounds) - paid for both with the workload's
memory, for a failure that was usually the host's, not the guest's.

What changed, and why each step is safe:

- **Asked again, with longer each time** (10 s, 20 s, 30 s; a minute in all, which is how long a
  wake can wait behind the sleep's lock). Seal is idempotent in execd - it sets `sealed` and
  forgets the last re-key, nothing else - so a 204 on any attempt proves the guest is sealed, and
  the sleep snapshots exactly as if the first answer had come. A starved guest given the same short
  bound again fails the same way; given longer, it answers.
- **Never paused first.** A paused VM runs no guest code; execd could not answer a Seal at all.
- **Never snapshotted unconfirmed.** A snapshot of an unsealed execd restores already serving with
  its identity - the thing sealing exists to prevent. "The answer was probably lost" is not proof.
- **Unconfirmed after every attempt: re-keyed and kept.** The guest's state is unknown - the last
  answer may be the one that was lost - and a re-key settles it: a 204 means execd is unsealed,
  holds a fresh secret (so no earlier secret can seal it), and serves. The VM stays up with its
  memory and the sleep returns `provider.ErrStillRunning`; the daemon keeps the unit awake, so its
  next idle check tries again. That is not "half-sealed": the one state v0.11 refused to leave up
  is exactly the one the re-key rules out.
- **Stopped as before when it cannot be settled, or will not be.** A re-key that fails too leaves
  execd possibly sealed, so the VM is stopped (next wake cold). And the third sleep in a row that
  the guest does not confirm stops it regardless (`maxSealStrikes`, counted in the record, reset by
  any snapshot): a guest can defer its sleep by about three idle intervals, not for ever, and holds
  no more than the memory it was already given while awake.

A memory snapshot of a running VM (`sbx snapshot` of a `sandbox.json` microVM) retries its Seal the
same way but still stops the VM when it stays unconfirmed: the caller asked for a snapshot and gets
an error either way. (An API sandbox's snapshot is its disk and seals nothing.)

### A firecracker snapshot restores only as itself

The VM state names its drives by host path, and the guest's IP was set by the kernel at first boot
and lives in its memory. So `sbx snapshot` on firecracker restores as the same sandbox and service
in the same slot, and refuses anything else by name. Anywhere else would be a fork: two VMs with the
parent's execd token and every key userspace made before the snapshot (the spike measured the token
identical in every clone at N=2..50). Forking needs per-clone drive paths (a jailer or mount
namespace) and the guest agent's re-key; until both exist it is refused, not approximated.

An OpenSandbox API sandbox's snapshot is different in kind, not an exception: it is its disk alone,
and a sandbox made from it is a new VM cold-booted from a copy (see "The OpenSandbox API on a
microVM"). No memory is forked, so nothing above applies to it.

### Guest networking is arithmetic, and has no way out

**Status:** amended in v0.12.0: the filter listens on VM bridges and sbx writes INPUT rules for its own bridges. The first paragraph describes v0.11.

Each sandbox gets a bridge, `sbxfc<slot>` on `10.231.<slot>.0/24`, and each service a tap and the
address `.<port index + 2>`. Computed, never leased, because Create, the daemon's Start and a restore
after a reboot can each be a different sbx process and all must agree without talking. sbx writes
no iptables rule, so nothing masquerades and nothing leaves - the no-NAT model `egress: "deny"`
already uses - and `egress_allow`/`egress_policy` are refused until the filter listens on a VM bridge.
Between bridges the host routes only if `ip_forward` is on and FORWARD allows it; docker sets that
policy to DROP, and `sbx doctor` shows `ip_forward` rather than sbx writing a rule to be sure.

*Amended in v0.12.* The filter now listens on a VM bridge, so `egress_allow`/`egress_policy`/`egress:
"allow"` are no longer refused, and sbx now writes firewall rules - INPUT only, for its own bridges
only. Both are the next entry. FORWARD is still docker's, and still only reported.

### A microVM's only door is its filter, and the host behind it is closed

**Status:** current; fails closed since v0.13.0, superseding v0.12's boot-and-warn.

A filtered microVM gets exactly what a filtered container gets: the daemon's egress filter, on its
bridge's gateway (`10.231.<slot>.1:20999`), as `HTTP(S)_PROXY` in the environment fc-init hands
execd and the workload - and still no route of its own, because the bridge still has no NAT. So
`egress_allow`, `egress_policy`, `egress: "allow"`, live `PUT`/`PATCH`/`DELETE` through
`EgressControl`, CIDR and wildcard rules, and "traffic through the filter is activity" are the
docker behaviours unchanged, served from the one place a VM's traffic can go. `egress: "allow"` is
the same door with an open default, and costs raw TCP exactly as it does on docker.

Three things are different, because here the filter and the host are the same machine.

**The filter binds before the bridge exists** (`IP_FREEBIND`). A reboot takes the bridge, and the
first wake remakes it; a filter that could bind only once the address existed would miss that
wake's first requests. Measured in a colima helper VM: bridge and tap deleted, daemon restarted -
the filter was listening on `10.231.0.1:20999` with no `sbxfc0` link, holding the live policy.

**The filter refuses its own host.** Under an open default a filter on the host would carry a guest
to `10.231.<slot>.1:22` - the host's sshd - or to another sandbox's guest, neither of which the
guest can reach itself. A VM's filter refuses `10.231.0.0/16`, every address on the host's
interfaces, loopback and link-local.

**No rule in the sandbox's policy opens that refusal** (security review of v0.12, H2). It first
shipped overridable, like the policy's own loopback default: an allow rule naming the address opened
it. But the policy is the sandbox's - its API caller writes it - and the filter dials as the root
daemon, so `allow 0.0.0.0/0`, `allow 10.0.0.0/8` or `allow 127.0.0.1` let a sandbox CONNECT to the
host's loopback (the OpenSandbox API, the daemon's ports), the host's addresses and other guests.
Now `Filter.Refuse` is absolute; widening it is an operator setting on `sbx serve`, never a policy
rule. A docker filter the daemon hosts on the host gets the same treatment for loopback and
link-local (`egress.HostLocal`) - its loopback is the host's too; a filter running in its own
container keeps the overridable default, since its loopback is its own.

**A VM's filter refuses private ranges and the host's subnets by default** (security review of
v0.12, M2 and S4). `egress: "allow"` on a VM carried a guest to docker container IPs
(`172.16.0.0/12`), the host's LAN and the cloud VPC - none reachable from a no-NAT bridge, all
reachable through a filter that dials as the host. Now a VM's filter (`egress.VMRefuse`) also
refuses RFC 1918, CGNAT (`100.64.0.0/10`), IPv6 ULA (`fc00::/7`), `0.0.0.0/8`, and every address on
the prefix of any host interface (`Contains`, not equality - the LAN `/24`, the VPC subnet, a
docker bridge's containers), on top of link-local. No sandbox policy opens it. The operator can:
`sbx serve --vm-egress-allow 10.20.0.0/16` (or `SBX_VM_EGRESS_ALLOW`) lifts the private and subnet
layer for the ranges named - never the host's own addresses, its loopback or the `10.231.0.0/16`
plan. Docker's filters are unchanged here: a container on docker's bridge reaches those ranges
through docker's own routing anyway, so refusing them in its filter would close nothing.

**The host's INPUT chain is closed to the bridge, except the filter port** (SECURITY.md M3). The
network entry above said sbx writes no rule, and the docker entry rejected rules in `DOCKER-USER` as
sbx reaching around docker. Neither argument applies here: this bridge is sbx's, made and deleted by
sbx with nobody else's rules on it, and the exposure - every host service bound to `0.0.0.0` - is
one a VM user cannot see from inside the spec. So each `sbxfc<slot>` gets a chain `SBX-FC<slot>`
(replies `RETURN` to the host's own rules, the filter port `ACCEPT`, everything else `DROP`) and one
jump at the top of INPUT matched to that bridge by name, and IPv6 is switched off on the bridge so
a guest's link-local address has nothing to talk to. What makes it safe to own:

- **Scoped by name.** Nothing outside the `SBX-FC<slot>` chains and the rules that match `sbxfc<slot>`
  is read, written or reordered; a host with no microVM sandbox has no rule of sbx's.
- **Made with the bridge, before it is up; removed with it** - and collected by `RemoveBridge` even
  when the bridge was deleted by hand. A failure halfway removes what was made: guarded, or exactly
  as before, never half a chain.
- **Fails closed (v0.13; supersedes "never a reason a sandbox will not boot").** No `iptables`, a
  rule refused, IPv6 not switched off, or a flushed guard that cannot be put back: the create or wake
  is refused with the reason and the fix, and a bridge made for it is deleted rather than left up
  unguarded. v0.12 booted anyway and warned; that was the right trade while the guard was new and
  sbx was a tool one operator ran, and the wrong one for an API that hands VMs to callers - a guest
  that finds the host open because a firewall module was missing is the outcome the guard exists to
  prevent, and a warning in a daemon log does not reach the person it matters to. The escape is an
  operator's statement, not a failure mode: `--fc-firewall=unmanaged` (`SBX_FC_FIREWALL`), under
  which sbx writes no rule at all and the host firewall is the operator's. A bridge already in use
  whose guard the daemon's reconcile cannot put back keeps its running VMs (killing them would be a
  second, silent failure) and is logged; no VM starts on it until the guard is whole.
- **Checked, not rewritten, on the wake path.** Made when the bridge is made; on every wake and every
  daemon reconcile, six `iptables -C` checks (each hook and each chain's final `DROP`) and no write
  while the guard is whole. A rule a firewall reload or `iptables -F` removed is put back on the next
  wake or within one refresh interval, and said so in the log. A repair leaves a chain that is
  still whole alone - flushing one that a hook still jumps to would open the bridge while it is
  empty. (Until v0.12 a flushed rule stayed gone until the sandbox was recreated; security review
  M4.)

Measured in the same helper VM, with a host listener on `0.0.0.0:18999`: the host reached
`10.231.0.1:18999`; the guest timed out on it, and got 403 asking the filter for it.

**INPUT alone did not close it: a docker-published port is not an INPUT packet** (security review
of v0.12, H1). Docker's nat `PREROUTING` DNATs every packet for a local address (`addrtype LOCAL`)
on a published port onto the container behind it. A guest dialling `10.231.<slot>.1:<published
port>` is therefore FORWARD traffic by the time the filter table sees it, and `DOCKER`'s chain
accepts it - `SBX-FC<slot>` in INPUT is never consulted. A container's own IP and a kube-proxy
NodePort are the same shape. So the guard has a second half, in `mangle`:

- `SBX-FC<slot>` in mangle (replies `RETURN`, `-p tcp -d <gw> --dport 20999` `RETURN`, the rest
  `DROP`), reached from the top of mangle `PREROUTING` by `-i sbxfc<slot>`. Mangle runs after
  conntrack, so the reply to a host's own dial is still known as one, and before nat, so a guest's
  packet is dropped before DNAT can rewrite it.
- `-i sbxfc<slot> -j DROP` and `-o sbxfc<slot> -j DROP` at the top of mangle `FORWARD`. Nothing is
  ever meant to be routed from or onto a guest bridge: it has no NAT, and the host reaches its
  guests as OUTPUT. This also makes isolation between sandboxes sbx's own rather than the host's
  FORWARD policy, wherever the guard is installed.

Mangle rather than filter `FORWARD` or `DOCKER-USER` because docker writes nothing to mangle: a
docker restart re-inserts its jumps at the top of filter `FORWARD`, which would put `DOCKER`'s
accepts back in front of ours. The two tables are installed and removed as one: every chain filled
before any rule jumps to one, and a failure anywhere removes what was made in both.

Verified two ways. The rule-set tests in `internal/fc` (`guard_forward_test.go`: the exact mangle
chain, the hooks, idempotence, and nothing left behind by a failure at any step) run against a
fake iptables that interprets the commands. `TestGuardLive` (`SBX_GUARD_LIVE=1`, root, Linux; the
CI `microvm` job runs it) rebuilds docker's shape by hand on the real kernel - a nat DNAT for local
addresses onto a namespace, a FORWARD accept - puts a guest namespace on an `sbxfc` bridge, and
checks that the guest reaches the published port and a host service before `Install` and neither
after, while the filter port and the host-to-guest direction still work. It cannot run on the
development Mac, so its first run is CI's.

**Rejected: nftables directly.** It is the better API, and docker - the other writer on every host
that runs this - still speaks `iptables`, which on current distributions is the nft backend anyway.
One tool, the one the operator already reads.

**Rejected: a firewall on the guest side** (rules inside the VM). The guest's root owns them.

### Every VMM runs under Firecracker's jailer, as its VM's own uid, jailed inside its VM's directory

**Status:** current; the network namespace per VMM, rejected for v0.13, was adopted in v0.14.0.

v0.12 started firecracker as a plain child of the root daemon: a guest-to-VMM escape landed as root
on the host (SECURITY.md, accepted for v0.12). v0.13 starts every VMM through **Firecracker's own
`jailer`**, on by default, because anonymous use of the OpenSandbox API on microVMs is not
defensible without it.

- **The jailer is pinned with firecracker, from the same v1.17.0 release tarball**, and verified by
  the same sha256 (the tarball's); it is cached in a directory of its own, so a v0.12 cache holding
  only firecracker is never taken for one with the jailer. Its interface was read from v1.17.0's
  source (`src/jailer/src/env.rs`) and `docs/jailer.md` at that tag, not from memory: `--id` is
  alphanumerics and hyphens up to 64; the chroot is `<canonical base>/<basename of the canonical
  exec file>/<id>/root`; it mknods `/dev/kvm`, `/dev/net/tun`, `/dev/urandom` and
  `/dev/userfaultfd` and fails EEXIST on a stale one; it closes every fd but 0-2, clears the
  environment, and without `--new-pid-ns`/`--daemonize` execs firecracker **in place** as
  `--id <id> --start-time-us ... <our args>` - so the pid sbx started, its reaper and its console
  and vmm log files all carry over unchanged.
- **uid = base + slot*256 + index** (base 900000, `SBX_FC_JAILER_UID_BASE`), gid the same.
  Arithmetic on the address, like the IP ("Guest networking is arithmetic"): every sbx process
  agrees on it with no table and no lock, a re-created VM gets its own back, two VMs never share
  one, and it is never 0. A pool was rejected for the reason addresses are not allocated. The tap
  is made for that uid; a tun device's owner cannot be changed, so one made for another uid (root's,
  by v0.12) is deleted and made again.
- **The jail is inside the VM's directory**: `<vm dir>/jail/firecracker/<id>/root`, `<id>` derived
  from the directory's path, so two state roots with the same ref never share a cgroup. Rejected:
  `<state>/jail`. Inside, everything that already removes a VM's directory (`sbx rm`, a failed
  create, the warm pool) removes its jail, and the hard links below stay on one filesystem.
- **What the root holds**: `/vmlinux` (a hard link when the kernel - symlink resolved - is
  root's, readable by all and writable by nobody else; a root-owned 0444 copy otherwise; never
  re-owned, since it is one inode for every VM, and re-checked and forced back to root's and
  read-only once the jailer has finished with the root); a layered VM's `/base.ext4` exactly as the
  kernel (v0.14: every VM of the image shares it); `/agent.ext4`, `/upper.ext4` (or `/rootfs.ext4`),
  `/vol<N>.ext4` and, for a restore, `/vm.state` and `/vm.mem` - hard links to the VM's own files,
  owned by its uid (except the drives it only reads - the agent drive and read-only volumes - kept
  root's and other-readable, so a compromised VMM cannot rewrite them for the next VM), so what the guest writes is on the VM's disk (a copy would be a disk the host never sees, so
  a VM's file that cannot be linked fails the launch); what the jailer adds (the binary, the device
  nodes); and the VMM's `/api.sock` and `/vsock.sock`. `<vm dir>/api.sock` and `vsock.sock` are
  symlinks into the root, so the API client, the vsock dialer and the wake proxy keep their paths,
  and the 108-byte socket limit applies to the short one.
  Dialled through that link, but not blindly: the entry in the root is the VMM's to replace, so it
  must be a socket owned by the jail's uid, and the process that answers must be that uid
  (`SO_PEERCRED`) - never a symlink to another VM's socket (fc.DialVMM).
- **Emptied on every launch and every Kill**, with the VM's cgroup (`<cgroup2>/sbx-fc/<id>`, which
  the jailer never removes): a stale `/dev/kvm` fails the next jailer, and a stale hard link to a
  replaced `vm.mem` would keep its blocks allocated.
- **The VMM is given paths in its root, and what it writes is taken back.** A sleep's snapshot is
  written at `/vm.state.new` and `/vm.mem.new` (or `/diff.mem`) and renamed into the VM's directory
  before the VMM ends; a commit's at `/commit.state` and `/commit.mem`, renamed into the snapshot's
  directory. Taken back only as a plain file with one name, checked after the rename in a directory
  only root writes: the VMM owns its root, and a symlink or hard link planted there would otherwise
  be followed by the host - merged into, cloned, overwritten - as root.
- **cgroup v2**: `cpu.max` = CPUs x 100000 per 100000, `memory.max` = the guest's memory + 128 MiB
  (the VMM's heap and the page cache of its drive and snapshot I/O, which reclaims under the limit
  rather than OOM-killing). No limits means no cgroup flags at all: v1.17 with `--cgroup-version 2`,
  no `--cgroup` and an existing `--parent-cgroup` *moves* the process into the parent, which fails
  once a sibling has enabled memory there.
- **`fsize` (RLIMIT_FSIZE, the jailer's `--resource-limit`), since the layered root.** v0.13 first
  shipped without one, reasoning that it would kill a VM for writing past that offset of its own
  disk. That holds only for a limit below the disk: a guest writes through a block device whose
  capacity IS its drive file's size, so it never writes past the largest drive. The limit is the
  largest file the VMM may write - its writable layer, a read-write volume, a memory snapshot of its
  RAM - plus 64 MiB, measured from the staged files at each launch (a volume made larger earlier
  still fits). It bounds each file a compromised VMM writes in the jail it owns; it does not bound
  how many (SECURITY.md).
- **A VM's record says how its running VMM was launched** (`jail_uid`, saved before the launch; a
  record without it is unjailed). Every call to that VMM - a snapshot, a commit - takes its paths
  from the record, not from this process's `SBX_FC_JAILER`, which only decides the NEXT launch: a
  v0.12 VMM slept by a jailed daemon would otherwise be told `/vm.state.new` and write its RAM into
  the host's `/`.
- **A snapshot records which kind it is** (`snapshot_jailed`): its drive paths are the root's or the
  host's, and a VMM of the other kind cannot open them. A wake with the jailer switched the other
  way cold-boots (disk kept, memory lost, said); a saved memory snapshot of the other kind is
  refused by name. Every v0.12 VM's first wake under v0.13 is therefore a cold boot.
- **A network namespace per VM** (`--netns`; *rejected for v0.13, adopted in v0.14*). The first
  cut kept the tap in the host namespace, reasoning a namespace would need a veth pair per VM "and
  a second guard". It needs the pair and not the guard: the guard's rules match on the sandbox's
  bridge (`-i sbxfcN`), and a frame from the guest now reaches that bridge through a veth port
  instead of a tap port - the same bridge, the same rules. So each VM's namespace
  (`/var/run/netns/sbxfc<slot>-<index>`) holds its tap - owned by the VM's uid - on a bridge `br0`
  with `eth0`, the inner end of a veth whose host end (named as the tap was) is a port of the
  sandbox's bridge; nothing inside has an address (IPv6 `addrgenmode none`) or a route. The jailer
  joins it before it drops privileges. What that buys: a VMM escape (its uid, no capabilities) has
  no interface on the host's network at all - no host loopback, no host service, no other link -
  only the guest's own tap, i.e. exactly what the guest could reach. It is rebuilt from nothing at
  every launch (a new namespace, tap and pair), so a tap's owner is always this launch's uid and no
  two VMMs ever open one tap. Unjailed (`SBX_FC_JAILER=off`) there is no namespace: only the jailer
  can put the VMM in one.

**Escape hatches, each named for what it gives up.** `SBX_FC_JAILER=off`, for a development host
where the jailer cannot run (no cgroup v2, no mknod): v0.12's behaviour exactly, warned on every
provider construction; the OpenSandbox API refuses to serve on firecracker with it unless
`--osb-insecure-no-jailer`. A value other than on or off is an error - a typo in an escape hatch
must not open it. `SBX_FC_JAILER_BINARY` replaces the pinned jailer as `SBX_FC_BINARY` does
firecracker; set both, since a jailer is meant for its own release's firecracker.

How it is proven:

- **unit-tested** against fakes: argv, uid arithmetic, staging by link and ownership, path
  translation, Adopt refusing a symlink, hard link or directory, and every path the provider hands
  the VMM resolving inside a root the fake enforces.
- `TestTheRealJailer` runs the real pinned jailer with the test binary in firecracker's place
  (root and cgroup v2, no KVM). It checks uid, capabilities, chroot, cgroup limits and release
  from /proc.
- **verified in CI** (the `microvm` job): `TestFirecrackerE2E` asserts the same of the real VMM
  every round, and every VMM the conformance tier causes is sampled from /proc.

### Warm-pool members wait asleep on a microVM, and each is its own VM

On `--provider firecracker` (Linux, `/dev/kvm`) a member is an ordinary API microVM of the pool
key, born running with execd healthy, then *parked* by the provider (`PoolParker`): by default
**asleep** - execd sealed, a Full snapshot, the VMM ended - so a waiting member holds no RAM and a
memory file as big as its RAM (`sbx doctor` says how much the pool holds); with
`--osb-pool-freeze`, **frozen** - Firecracker's pause, memory resident, no CPU. The opposite
default from docker's, because the trade is different: a waiting container costs `tail` and an
idle execd, a waiting VM costs its whole RAM.

A claim is one provider call under the VM's lock: restore (or resume), then one re-key carrying
the token the API minted for the request, the caller's env and a new control secret. Not
`POST /sbx/claim`: a restored execd is sealed and answers nobody until its host re-keys it over
vsock, so the re-key is the claim. The record takes the caller's token before the wake, so every
later restore re-keys with it; and a claimed VM that dies awake and cold-boots is re-keyed again
after the boot, because its agent drive still carries the member's token and none of the
caller's env (rebuilding that drive per claim would cost an mkfs on the claim path). A member
whose re-key fails is stopped and discarded, the cause logged, and the create goes cold - never
handed out. The daemon holds every member, so no connection can wake one with its own token.

Each member boots from its own disk, with its own drives, slot, tap, token and control secret:
no two share memory. Forking members from one template's snapshot would make the pool far
cheaper to fill, and is deliberately not this release - a memory clone carries the kernel-set
guest IP and every secret userspace made before the snapshot, and re-key replaces only execd's
("The OpenSandbox API on a microVM"). The helper-VM path on a Mac or Windows still refuses
`--osb-pool` at startup: the API there runs in the VM's daemon, whose provider could park
members, but the pool is not carried into the VM yet (see "The OpenSandbox API through a helper VM
runs in the VM").

A member is jailed like any VM (v0.13 ships the pool and the jailer together): every launch -
its boot, a frozen member's paused VMM, an asleep claim's restore into a fresh root - runs as the
uid of its address, and the claim's re-key reaches execd through `<dir>/vsock.sock`, the symlink
into that root. A member parked with the jailer one way and claimed with it the other is refused
at the claim, not loaded: its snapshot names drive paths the VMM cannot open, and a member that
would need a cold boot is no warmer than a cold create.


---

## Measuring

### What makes two wake numbers comparable

`scripts/compare.sh` publishes numbers from different tools on one machine — meaningful only
under rules decided in advance. These are those rules, written with the first table rather than
after someone disputes one.

**A sample counts only on a correct protocol reply** — not a status code, not a connection, but
a `PONG`, a body, a row. Sablier's middleware failed to engage during development and returned
502 in 98 ms, faster than sbx's real wake; a benchmark accepting status codes would publish a
rival's failure as its best result.

**A sample is void unless the target was verifiably asleep when the clock started.** Otherwise a
contender whose mechanism never engaged scores a spectacular wake for answering while already
awake. Void samples are counted and printed beside n.

**Every wake is paired with a baseline through the identical client, and the compared quantity
is the paired difference.** The arms do not share a network path — sbx publishes on the host,
docker-hosted rivals from inside the VM — and roughly 100 ms of an early 336 ms "wake" was
`curl` starting up. Statistics are computed on the paired differences; a p90 of an unpaired
subtraction is undefined and must never appear.

**Below n=10 a row reports min/median/max, never p90**, because a nearest-rank p90 over five
samples is the fourth-highest value wearing a percentile's name. `BENCHMARKS.md` already
follows this for its n=5 kubernetes row.

**A delta smaller than the harness's own jitter is not published as a number.** The floor is
measured direct-vs-direct — the same client against the same directly published target, twice —
and anything inside it reports "below harness resolution" instead of a figure the instrument
invented.

**Three statuses, because they are three different facts.** `N/A` means the contender cannot do
this by design and is a result: Sablier has no postgres row because it is HTTP-only. `SKIPPED`
means it could not be stood up here and is not a result. **No row at all** means it cannot be
gated — a claim with a shelf life, which zeropod illustrates. It checkpoints while the pod stays
`Running`, so nothing in `kubectl get pod` separates asleep from awake, which left it
unmeasurable under this rule. The answer is a different observable rather than none:
`zeropod_running` is 0 while checkpointed, and gating on that produces a real 272 ms
measurement. "Cannot be gated" is a statement about the gate you have looked for, so it belongs
in a document that expects to be revisited.

**Rows in different categories are not ranked against each other.** Disk-warm and RAM-restored
are different quantities; every row carries what comes back.
