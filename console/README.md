# console

Metrics, health and a read-only API for a running `sbx` daemon.

```sh
go build -o console . && sbx serve 2>&1 | ./console
console  http :49722  metrics :49723/metrics  health :49722/.well-known/health
```

**A separate module on purpose.** The root `go.mod` has no dependencies and that is a product
claim, not an accident — `go install github.com/aryanmehrotra/sbx@latest` still resolves
nothing but the standard library, and the daemon stays small because nothing was linked into
it that did not have to be. This module requires GoFr; the one it watches requires nothing.

**Ports are discovered, never GoFr's defaults.** GoFr refuses to start when a port is taken,
and a fixed `8000/2121/9000` is exactly the sort of thing already in use on a developer's
machine — or in use by a second console watching a daemon on another host. The ports are
chosen at startup and printed. (One `sbx serve` per machine: it owns every sandbox's public
ports, and a second refuses to start. One console per daemon.)

**It cannot slow anything down.** The daemon does not know this exists. It already writes a
structured JSON line for every wake and sleep, and this reads that stream — no callback, no
shared state, no socket between them, so there is no mechanism by which observability could
reach the byte-splice path.

| | |
|---|---|
| `/metrics` | `sbx_wakes_total`, `sbx_sleeps_total`, `sbx_wake_failures_total`, `sbx_wake_duration_ms` — all labelled by sandbox and service |
| `/.well-known/health`, `/alive` | auth-exempt, so Kubernetes probes need no credentials |
| `/api/sandboxes` | what the daemon is fronting, read-only |

⚠️ **Nothing here is authenticated** — not `/metrics`, not `/api/sandboxes`. It is a
read-only view of what the daemon on this machine is fronting, and it is meant to be bound to
loopback or a private network. Do not put it on a public address.

Telemetry is off: `GOFR_TELEMETRY=false` is set before the app starts. A tool whose job is
watching your infrastructure should not be the thing making an outbound call you did not ask
for, and it has to work on a machine with no network at all.
