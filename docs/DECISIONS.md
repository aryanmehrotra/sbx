# Decisions

> **Short version:** why sbx is shaped the way it is. Mostly these are things that broke, and
> the reasoning that replaced them. If you are about to change how sandboxes are addressed,
> woken or slept, the answer to "why not just..." is probably here.

Why this is shaped the way it is. Each of these was a real fork, and most were settled by
something breaking rather than by argument.

---

### There is no `start` and no `stop`

Whatever can start a sandbox eventually becomes the thing that left one running, and then
two components believe they own the lifecycle. Only the daemon owns it, because only the
daemon can see demand.

This is why the build harness integration has a readiness predicate and no `up`: **asking is
starting**, so there is nothing left for a second component to own.

---

### Bytes, not connections

A Go service's pool holds sockets open indefinitely. A sandbox fronted by a running service
would never be idle by connection count, and would never sleep.

---

### Ask the workload, not the platform

Docker republishes container health on its check interval. That lag was **98% of a wake** -
5030 ms against a Redis that was serving in 110 ms. The wake path runs the declared health
command itself; the reaper still asks the cheap, lagging question, because being a few
seconds late to *sleep* something costs nothing.

---

### A published port is not readiness

Docker binds the host side of `-p` the instant a container starts. Measured: the port
answered at **139 ms**, the server needed about a second more, and the client spliced in
between died reading the handshake. Services declare a health check and the daemon asks the
container.

---

### Slots are allocated, not hashed

Hashing branch names into 60 slots looks stable and collided on the first six names tried -
`auth-flow` and `naveen-reveiw`. Two sandboxes on one slot fight over ports. Docker labels
are the registry, so nothing can drift from reality.

---

### Optional services still reserve their ports

Skipping an optional ClickHouse used to shift MySQL's ordinal, so adding it later moved the
database out from under every config that had recorded where it was.

---

### A sandbox cannot sleep until it has been seen serving

The activator scaled a sandbox to zero **39 seconds into its own creation**, while the
command creating it was still waiting for the first health check. "Idle" is meaningless
before a service has ever been up: a sandbox pulling an image and running migrations looks
exactly like one nobody has touched.

---

### Three containers, not one image with everything in it

One image is simpler to reason about, and wrong here. Once waking is automatic, splitting is
*cheaper*: a branch that never queries the analytics store never pays for it. Merging them
means waking ClickHouse to read a config row.

---

### Tunnels are shelled out, and the anonymous one is opt-in

Cloudflare reached the same conclusion about their own SDK in 2026 and replaced
`exposePort()` with Cloudflare Tunnel.

The first version fell through to an anonymous third party automatically when ngrok failed.
Failing toward *less* trust is the wrong direction for a default, so `--via ssh` must now be
typed. It also uses `StrictHostKeyChecking=yes` rather than `accept-new`, and admits in its
own note that the operator publishes no fingerprint to pin against.

---

### Isolation fails closed, and says why

Asking for a runtime the machine lacks never silently downgrades you. Docker refuses
immediately. Kubernetes also refused - but silently, taking two minutes to report that the
service "never became ready" when the actual problem was a missing RuntimeClass. It now
checks first and says so in one second.

### What makes two wake numbers comparable

`scripts/compare.sh` publishes numbers from different tools measured on one machine, which
is only meaningful under rules decided in advance. These are those rules, written with the
first table rather than after someone disputes one.

**A sample counts only on a correct protocol reply.** Not a status code, not a connection -
a `PONG`, a body, a row. Sablier's middleware failed to engage during development and
returned 502 in 98 ms, faster than sbx's real wake. A benchmark that accepts status codes
publishes a rival's failure as its best result.

**A sample is void unless the target was verifiably asleep when the clock started.** A
contender whose mechanism never engaged otherwise scores a spectacular wake for answering
while it was already awake. Void samples are counted and printed beside n.

**Every wake is paired with a baseline through the identical client, and the compared
quantity is the paired difference.** The arms do not share a network path - sbx publishes on
the host, docker-hosted rivals from inside the VM - and roughly 100 ms of an early 336 ms
"wake" turned out to be `curl` starting up. Statistics on the delta are computed on the
paired differences; a p90 of an unpaired subtraction is undefined and must never appear.

**Below n=10 a row reports min/median/max, never p90**, because a nearest-rank p90 over five
samples is the fourth-highest value wearing a percentile's name. `BENCHMARKS.md` already
followed this for its n=5 kubernetes row.

**A delta smaller than the harness's own jitter is not published as a number.** The floor is
measured direct-vs-direct - the same client against the same directly published target,
twice - and anything inside it reports "below harness resolution" instead of a figure the
instrument invented.

**Three statuses, because they are three different facts.** `N/A` means the contender cannot
do this by design and is a result: Sablier has no postgres row because it is HTTP-only.
`SKIPPED` means it could not be stood up here and is not a result. **No row at all** means it
cannot be gated - and that is a claim with a shelf life, which zeropod proved. It checkpoints
while the pod stays `Running`, so nothing in `kubectl get pod` separates its asleep from its
awake, and it sat here as unmeasurable for exactly that reason. The answer was a different
observable rather than no observable: `zeropod_running` is 0 while checkpointed, and gating on
that produced a real 272 ms measurement. "Cannot be gated" is a statement about the gate you
have looked for, so it belongs in a document that expects to be revisited.

**Rows in different categories are not ranked against each other.** Disk-warm and
RAM-restored are different quantities; every row carries what comes back.

### A snapshot is the volume, not the container

`docker commit` is the obvious way to save a sandbox and it is the wrong one: it does not
capture mounted volumes. Committing a seeded postgres produced an image whose data directory
held **zero files** against the live container's twenty-four, and both forks came up blank
with a working server and an empty database - the worst shape of failure, because it looks
like it worked.

Everything worth snapshotting in sbx is in a volume. `volume` is the field that makes
sleeping safe, so it is by construction where state lives.

So a snapshot copies volume to volume through a throwaway container, which is docker's own
recipe and the right one here: it stays in docker's storage, needs no host path - colima
would not share one anyway - preserves ownership, which postgres requires before it will
start on a data directory at all, and never streams the bytes through the sbx process. The
image is still committed, for services that keep state outside a volume.

The restore happens **after** create and **with the service stopped**. Create starts each
service to health-check it, so a database has already initialised an empty data directory by
then; writing over that while it runs is replacing the floor underneath it. `init` is dropped
from a forked spec for the same class of reason: it has already run in the state being
forked, and running it again re-seeds a seeded database.

And the fork keeps its own `volume` declaration. The first implementation deleted it, on the
theory that the image carried the data - which is exactly the assumption that was wrong.

### Capabilities are negotiated, not stubbed - and sbx does not reach around a provider

Snapshot support arrived as four new methods on the core `Provider` interface, and
kubernetes was made to implement all four as stubs that return errors. That is not an
interface. A method on `Provider` is a promise every backend keeps, and four methods only
docker can keep is a docker client with a kubernetes-shaped hole in it.

They are an optional `Snapshotter` interface now. A provider implements it if it can do the
thing natively; the CLI asks with a type assertion and reports one refusal naming the
backend. It is the negotiation `--isolation` already uses - declare what you want, be told
plainly when this backend cannot give it.

The naming rule that follows: **a capability is named for what the user wants, never for how
a backend does it.** `Snapshotter`, not `Committer` - the kubernetes answer is a volume
snapshot through its own CSI, not `docker commit`, and an interface named after docker's verb
would have made the correct implementation look like a workaround.

**And sbx does not reach around a provider to do something the provider cannot.** Egress
control was nearly implemented by having sbx launch a privileged container to write
`DOCKER-USER` iptables rules on the host. It would have worked on this laptop. It is also
sbx mutating a host firewall from outside the abstraction it claims to have - invasive,
unverifiable on a machine where it cannot be tested, and true only while docker happens to
be arranged in one particular way.

The provider-neutral shape is a spec field saying *what* is wanted - deny egress - with each
backend implementing it natively or refusing: NetworkPolicy in a cluster, and for docker a
primitive that does not currently exist, since `--internal` and `--network none` both stop
port publishing and make a sandbox that can never be woken.

### Egress is denied by a bridge without NAT, not by a firewall sbx writes

`egress: "deny"` puts the service on a per-sandbox bridge created with
`com.docker.network.bridge.enable_ip_masquerade=false`. No masquerade means no NAT off the
host, so nothing routed leaves - and docker still publishes ports into that bridge, so the
wake path is untouched. Measured: published port answered 200, an outbound fetch was blocked,
and the service still slept and woke on a connection.

Two rejected approaches, both tried:

**`--internal` and `--network none`** block egress and also stop docker publishing the port
at all, so the sandbox can never be woken. A security control that breaks the thing it
protects will be turned on by someone who then trusts it.

**iptables rules in the `DOCKER-USER` chain**, applied by sbx launching a privileged
container. This would have worked on the machine it was written on. It is also sbx reaching
around the abstraction it claims to have and mutating a host firewall - invasive, untestable
where it cannot run, and correct only while docker is arranged one particular way. The bridge
option asks docker to do it, which is the difference between configuring a backend and
operating on the host behind its back.

The kubernetes provider refuses the field rather than ignoring it. Its answer is a
NetworkPolicy, only some CNIs enforce them, and a control that silently did nothing is worse
than one that says no.

**What it cannot do.** Every rival allows and denies **by domain, CIDR and IP** - E2B by
wildcard, Daytona as a firewall. This is all-or-nothing, and closing that gap needs something
that terminates or inspects connections:

- a **filtering proxy** the sandbox is pointed at, which means TLS termination or SNI
  inspection, a certificate the workload trusts, and a process that is not 0 B at rest;
- or **rules in the `DOCKER-USER` chain** matched to the container's address, which works on
  Linux and must run inside the VM on macOS - a capability that would have to degrade with a
  reason where it is absent, exactly like `--isolation gvisor|kata`.

Neither is a flag, which is why the first attempt at one was reverted rather than shipped.
Coarse is a real control and a coarse one, and COMPARISON.md scores it that way.

### sbx is a tool people run, not a service anyone offers

The obvious next step from a control plane is multi-tenancy: authentication, per-user
isolation, quotas, and eventually somebody hosting it. That is not the direction.

sbx exists to be adopted into other people's workflows - a binary they run on hardware they
already have, for their own branches, agents and CI. It is not trying to become the thing you
buy instead of E2B, and the comparison tables should be read that way: they explain which tool
fits a job, not which vendor wins.

Three consequences, so this is a decision rather than a mood:

**No auth, no tenancy, no quota**, and the README says so where someone might deploy it
anyway. A shared box for a team that trusts each other is the supported shape.

**The GoFr console is for the operator, not for tenants.** Metrics, health and a read-only
view of what the daemon is doing. It is not the seam through which sbx becomes hosted, and
the API stays read-only for that reason as much as for the lifecycle one.

**"Hosted Postgres, operated for you" stays in the use-something-else table permanently.**
Neon is the answer there and always will be - not because sbx cannot branch and scale to
zero, but because "somebody else runs it" is the whole product and this one is run by you.

---

### A built image is keyed by its content, never by its age

`build:` names the image it produces `sbx-build-<sha256 of the context>`, so the second
create with an unchanged context finds the image already there and does no build at all.

The alternative - and what [Daytona documents][daytona-builder] - is to expire the cache on a
timer: "Declarative images are cached for 24 hours ... subsequent runs **on the same runner**
will be almost instantaneous." Note the last clause; a content hash does not care which runner
it is on. A clock is wrong in both directions at once: it rebuilds a context that has not changed
since yesterday, and it reuses one that changed five minutes ago if the entry is still young.
Content-addressing has neither failure. Change a byte and the tag changes; change nothing and
the tag is the same next month.

What goes into the hash is the part that decides whether this works in practice:

**Not timestamps.** A fresh `git clone` rewrites every mtime, so an mtime-keyed cache misses
on every CI runner - which is precisely the machine where the cache is worth the most, and
the one where the developer never sees it failing.

**File modes, yes.** A script that stops being executable is a different image. Hashing only
contents would make that a silent cache hit that fails at runtime, which is worse than a
rebuild.

**`.git` and `node_modules`, no.** Otherwise the tag changes on every commit whether or not
any build input did, and the cache never hits twice.

**Symlinks are skipped**, since the target is either already inside the context or outside
it, and following one out of the context would put the host's filesystem into the key.

`image` and `build` together is an error rather than a precedence rule. Whichever one we
picked, half of all readers would guess the other, and the cost of guessing wrong is running
an image the file does not appear to describe.

Docker only. Building in a cluster means pushing to a registry the nodes can pull from -
credentials, a registry address, a retention policy - none of which sbx can assume without
becoming an opinionated CI system. `BuilderFor` refuses on kubernetes and says why, which is
the same negotiated-capability rule as snapshots.

---

### Adding an optional spec field does not bump `version`

The tempting rule is that every new field is a new format version. It is the wrong one here,
because `ParseSpec` already sets `DisallowUnknownFields`. An older binary meeting a newer
spec says:

```
sbx: sandbox.json: json: unknown field "build"
```

which names the field that is not understood. Bumping to `"version": 2` would replace that
with `unsupported version 2 (this build understands 1)` - strictly less information - and
would force every existing spec file to be edited even when it uses nothing new.

So `version` is reserved for a change that would be **silently misread**: a field that
changes meaning, a default that flips, a structure that is re-shaped. Optional additions are
not that, and the decoder already refuses them by name.

---

### Template images are pinned by digest, and the pin has a visible date

`zenika/alpine-chrome:latest` meant the first thing a new user ran could break without a
commit touching this repo, and the failure would read as "sbx is broken" rather than "the
upstream image moved". Every template image is now `name:tag@sha256:...`.

The tag is kept beside the digest deliberately. Docker resolves by digest and ignores the
tag, so the tag costs nothing and tells a reader what they are running; a bare digest tells
them nothing.

**A digest is only accepted if it names a manifest list covering linux/amd64 and
linux/arm64.** `scripts/pin-templates.sh` refuses otherwise. Pinning an arch-specific
manifest resolved on a laptop produces templates that pull there and fail in CI, and that
failure looks like a broken template rather than a bad pin - which is the worst kind, because
it sends whoever hits it to the wrong file.

Pinning buys reproducibility and pays for it in staleness: these images stop receiving
updates until somebody refreshes them. That is only an honest trade if the age is visible,
so `sbx templates` prints the refresh date and `examples/pinned.json` carries it. A pin whose
age nobody can see is a pin nobody ever refreshes.

There is no `make templates-refresh` because there is no Makefile - this repo puts its
tooling in `scripts/`, and adding a build system for one target would be the second way to
do something.

### `prewarm` is a separate command, not something `create` does

The first create on a cold machine is mostly a download. Folding a pull into create would
make every create's timing depend on whether the machine happened to be warm, which is
exactly the ambiguity the wake measurements exist to avoid.

`sbx prewarm` is instead a step CI can cache, and it reports what it skipped rather than
showing a spinner: a warm cache should print `0 pulled, 5 already present`, and a run that
pulls when it should not is the cache being broken. That line is the only thing worth
reading in the step's log.

Docker only, via a `Puller` capability. In a cluster there is no local image store to warm -
the image has to be on whichever node the scheduler later picks, which means a DaemonSet sbx
would be creating in somebody's cluster uninvited.

[daytona-builder]: https://www.daytona.io/docs/en/declarative-builder/
