# Decisions

> **Short version:** why sbx is shaped the way it is, and why not the obvious alternative. If
> you are about to change how sandboxes are addressed, woken or slept, "why not just..." is
> probably answered here.

Each entry states a decision and the reasoning behind it — including, where it matters, why the
obvious alternative doesn't hold up.

---

### There is no `start` and no `stop`

Whatever can start a sandbox becomes the thing that left one running, and then two components
believe they own the lifecycle. Only the daemon owns it, because only it can see demand. So the
build harness integration has a readiness predicate and no `up`: **asking is starting**, leaving
nothing for a second component to own.

---

### Bytes, not connections

A Go service's pool holds sockets open indefinitely, so a sandbox fronted by a running service
is never idle by connection count and never sleeps.

---

### Ask the workload, not the platform

Docker republishes container health only on its check interval, which lags the workload itself —
measured at **98% of a wake**: 5030 ms against a Redis serving in 110 ms. So the wake path runs
the declared health command itself, rather than asking docker; the reaper still asks the cheap,
lagging question, because being seconds late to *sleep* something costs nothing.

---

### A published port is not readiness

Docker binds the host side of `-p` the instant a container starts, before the process behind it
is ready to serve — measured at **139 ms** for the port to answer against about a second more
before the server itself was ready, with a client spliced in between dying while reading the
handshake. So services declare a health check, and the daemon asks the container rather than
trusting the port.

---

### Slots are allocated, not hashed

Hashing branch names into 60 slots looks stable and collides in practice — `auth-flow` and
`naveen-reveiw` collide within the first six names tried. Two sandboxes on one slot fight over
ports, so slots are allocated instead, with docker labels as the registry: nothing can drift from
reality because there is no derived mapping to drift from.

---

### Optional services still reserve their ports

Skipping an optional service must not shift the ordinal of the ones after it — adding ClickHouse
later would otherwise move the database out from under every config that had recorded where it
was. So a service's ordinal is fixed whether or not it is actually present.

---

### A sandbox cannot sleep until it has been seen serving

"Idle" is meaningless before a service has ever been up: a sandbox pulling an image and running
migrations looks exactly like one nobody has touched. Left ungated, that reading is reachable
fast — scaling to zero **39 seconds into creation**, while create is still waiting on the first
health check, is well within range. So a sandbox is not eligible to sleep until it has been seen
serving at least once.

---

### Three containers, not one image with everything in it

One image is simpler to reason about, and wrong here. Once waking is automatic, splitting is
*cheaper*: a branch that never queries the analytics store never pays for it. Merging them means
waking ClickHouse to read a config row.

---

### Tunnels are shelled out, and the anonymous one is opt-in

Cloudflare reached the same conclusion about their own SDK in 2026, replacing `exposePort()`
with Cloudflare Tunnel.

Falling through to an anonymous third party automatically the moment ngrok fails is the wrong
default — failing toward *less* trust should never happen silently. So `--via ssh` must be typed
explicitly. It uses `StrictHostKeyChecking=yes` rather than `accept-new`, and admits in its own
note that the operator publishes no fingerprint to pin against.

---

### Isolation fails closed, and says why

Asking for a runtime the machine lacks never silently downgrades you. Docker refuses
immediately. Kubernetes' own default is to refuse silently, taking two minutes to report the
service "never became ready" when the real problem is a missing RuntimeClass — so sbx checks
first and says so in one second, rather than letting that report stand in for a diagnosis.

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
root. They are mounted with `--mount`, not `-v`: `-v` creates a missing bind source, and on a Mac
it creates it inside the runtime's VM, where the caller's files are not. Refused is the useful
failure.

`pvc` is a docker named volume, as upstream's docker runtime makes it - but named
`sbx-osb-pvc-<claimName>`, where upstream uses the claim name verbatim. Verbatim would let any API
caller mount any volume on the engine by naming it: another sandbox's database, the execd volume,
something that has nothing to do with sbx. `deleteOnSandboxTermination` removes only a volume that
create made, never one that already existed, and `sbx gc` never treats these as orphans - they are
a caller's storage with no sandbox to be orphaned from. A cluster answers 501 until a
PersistentVolumeClaim, whose storage class and size are the operator's decisions, is built.

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

### Egress is denied by a bridge without NAT, not by a firewall sbx writes

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
quotas, and upstream's key-to-namespace multi-tenancy is deliberately not implemented. It binds
loopback by default and refuses any other address without `--osb-key`. A team that wants
identities puts a gateway in front, exactly as this section already says.

**"Hosted Postgres, operated for you" stays in the use-something-else table permanently.** Neon
is the answer there and always will be — not because sbx cannot branch and scale to zero, but
because "somebody else runs it" is the whole product, and this one is run by you.

---

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

---

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

---

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

### Memory checkpoint goes through podman, because docker's restore path doesn't work

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

## Traffic through the egress filter counts as activity, stamped on bytes

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
---

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

Injecting `/opt/sbx/sbx` and an init into the image's ext4 would make every cached rootfs depend on
the sbx binary's hash, so rebuilding sbx - every dev build - would rebuild every image. Instead each
VM gets a small read-only **agent drive** (`vda`: `/sbx` and a 0600 `/init.json` with the entrypoint and
environment) that the kernel boots as root; `sbx fc-init` mounts the image (`vdb`), bind-mounts the
agent at `/opt/sbx/sbx`, switches root and becomes execd. It is an initramfs with a filesystem
instead of a cpio, so it costs no guest RAM. The rootfs is cloned per VM with `FICLONE` where the
filesystem can (btrfs, XFS) and a sparse copy where it cannot (ext4): 57 ms for a 256 MiB file
holding 3 MiB on APFS; the e2e test logs which one a Linux host got.

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

### A firecracker snapshot restores only as itself

The VM state names its drives by host path, and the guest's IP was set by the kernel at first boot
and lives in its memory. So `sbx snapshot` on firecracker restores as the same sandbox and service
in the same slot, and refuses anything else by name. Anywhere else would be a fork: two VMs with the
parent's execd token and every key userspace made before the snapshot (the spike measured the token
identical in every clone at N=2..50). Forking needs per-clone drive paths (a jailer or mount
namespace) and the guest agent's re-key; until both exist it is refused, not approximated.

### Guest networking is arithmetic, and has no way out

Each sandbox gets a bridge, `sbxfc<slot>` on `10.231.<slot>.0/24`, and each service a tap and the
address `.<port index + 2>`. Computed, never leased, because Create, the daemon's Start and a restore
after a reboot can each be a different sbx process and all must agree without talking. sbx writes
no iptables rule, so nothing masquerades and nothing leaves - the no-NAT model `egress: "deny"`
already uses - and `egress_allow`/`egress_policy` are refused until the filter listens on a VM bridge.
Between bridges the host routes only if `ip_forward` is on and FORWARD allows it; docker sets that
policy to DROP, and `sbx doctor` shows `ip_forward` rather than sbx writing a rule to be sure.
