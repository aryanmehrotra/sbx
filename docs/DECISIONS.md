# Decisions

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

Docker republishes container health on its check interval. That lag was **98% of a wake** —
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

Hashing branch names into 60 slots looks stable and collided on the first six names tried —
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
immediately. Kubernetes also refused — but silently, taking two minutes to report that the
service "never became ready" when the actual problem was a missing RuntimeClass. It now
checks first and says so in one second.

### What makes two wake numbers comparable

`scripts/compare.sh` publishes numbers from different tools measured on one machine, which
is only meaningful under rules decided in advance. These are those rules, written with the
first table rather than after someone disputes one.

**A sample counts only on a correct protocol reply.** Not a status code, not a connection —
a `PONG`, a body, a row. Sablier's middleware failed to engage during development and
returned 502 in 98 ms, faster than sbx's real wake. A benchmark that accepts status codes
publishes a rival's failure as its best result.

**A sample is void unless the target was verifiably asleep when the clock started.** A
contender whose mechanism never engaged otherwise scores a spectacular wake for answering
while it was already awake. Void samples are counted and printed beside n.

**Every wake is paired with a baseline through the identical client, and the compared
quantity is the paired difference.** The arms do not share a network path — sbx publishes on
the host, docker-hosted rivals from inside the VM — and roughly 100 ms of an early 336 ms
"wake" turned out to be `curl` starting up. Statistics on the delta are computed on the
paired differences; a p90 of an unpaired subtraction is undefined and must never appear.

**Below n=10 a row reports min/median/max, never p90**, because a nearest-rank p90 over five
samples is the fourth-highest value wearing a percentile's name. `BENCHMARKS.md` already
followed this for its n=5 kubernetes row.

**A delta smaller than the harness's own jitter is not published as a number.** The floor is
measured direct-vs-direct — the same client against the same directly published target,
twice — and anything inside it reports "below harness resolution" instead of a figure the
instrument invented.

**Three statuses, because they are three different facts.** `N/A` means the contender cannot
do this by design and is a result: Sablier has no postgres row because it is HTTP-only.
`SKIPPED` means it could not be stood up here and is not a result. **No row at all** means it
cannot be gated — zeropod checkpoints while the pod stays `Running`, so nothing distinguishes
its asleep from its awake, and a dash in a table would read as a bad day rather than as the
absence of a measurable property.

**Rows in different categories are not ranked against each other.** Disk-warm and
RAM-restored are different quantities; every row carries what comes back.

### Network egress control needs a proxy, not a flag

Every rival ships egress controls — E2B allows and denies by domain, CIDR and IP; Daytona
calls it a firewall. sbx has none, and a first attempt to add one as a spec field was
reverted rather than shipped, because docker's own network modes cannot provide it here.

Measured, 2026-08-15:

```
   container on --internal network, with -p     no port mapping at all, curl 000
   container on --network none,     with -p     no port mapping at all, curl 000
   container on a normal bridge,    with -p     HTTP 200
```

Both modes do block egress. Both also stop docker publishing the port — and the published
port is how the daemon reaches the service after waking it, so a sandbox with either mode
set is one that can never be woken. A security control that silently breaks the thing it
protects is worse than none, because it will be turned on by somebody who then trusts it.

What it actually takes is one of:

- **a filtering proxy in the sandbox's path**, which is a component with a lifecycle, not a
  flag — and it has to be on the wake path, which is the one place this project refuses to
  put anything that parses;
- **rules in the `DOCKER-USER` chain** matched to the container's address. That works on
  Linux and has to run inside the VM on macOS, so it is a capability that must degrade with
  a reason where it is absent, exactly like `--isolation gvisor|kata`.

Until one of those exists the honest position is the one in COMPARISON.md: this is a gap,
it is a security property rather than a convenience, and no field pretends otherwise.

### A snapshot is the volume, not the container

`docker commit` is the obvious way to save a sandbox and it is the wrong one: it does not
capture mounted volumes. Committing a seeded postgres produced an image whose data directory
held **zero files** against the live container's twenty-four, and both forks came up blank
with a working server and an empty database — the worst shape of failure, because it looks
like it worked.

Everything worth snapshotting in sbx is in a volume. `volume` is the field that makes
sleeping safe, so it is by construction where state lives.

So a snapshot copies volume to volume through a throwaway container, which is docker's own
recipe and the right one here: it stays in docker's storage, needs no host path — colima
would not share one anyway — preserves ownership, which postgres requires before it will
start on a data directory at all, and never streams the bytes through the sbx process. The
image is still committed, for services that keep state outside a volume.

The restore happens **after** create and **with the service stopped**. Create starts each
service to health-check it, so a database has already initialised an empty data directory by
then; writing over that while it runs is replacing the floor underneath it. `init` is dropped
from a forked spec for the same class of reason: it has already run in the state being
forked, and running it again re-seeds a seeded database.

And the fork keeps its own `volume` declaration. The first implementation deleted it, on the
theory that the image carried the data — which is exactly the assumption that was wrong.

### Capabilities are negotiated, not stubbed — and sbx does not reach around a provider

Snapshot support arrived as four new methods on the core `Provider` interface, and
kubernetes was made to implement all four as stubs that return errors. That is not an
interface. A method on `Provider` is a promise every backend keeps, and four methods only
docker can keep is a docker client with a kubernetes-shaped hole in it.

They are an optional `Snapshotter` interface now. A provider implements it if it can do the
thing natively; the CLI asks with a type assertion and reports one refusal naming the
backend. It is the negotiation `--isolation` already uses — declare what you want, be told
plainly when this backend cannot give it.

The naming rule that follows: **a capability is named for what the user wants, never for how
a backend does it.** `Snapshotter`, not `Committer` — the kubernetes answer is a volume
snapshot through its own CSI, not `docker commit`, and an interface named after docker's verb
would have made the correct implementation look like a workaround.

**And sbx does not reach around a provider to do something the provider cannot.** Egress
control was nearly implemented by having sbx launch a privileged container to write
`DOCKER-USER` iptables rules on the host. It would have worked on this laptop. It is also
sbx mutating a host firewall from outside the abstraction it claims to have — invasive,
unverifiable on a machine where it cannot be tested, and true only while docker happens to
be arranged in one particular way.

The provider-neutral shape is a spec field saying *what* is wanted — deny egress — with each
backend implementing it natively or refusing: NetworkPolicy in a cluster, and for docker a
primitive that does not currently exist, since `--internal` and `--network none` both stop
port publishing and make a sandbox that can never be woken.
