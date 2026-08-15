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
