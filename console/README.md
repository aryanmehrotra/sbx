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

**Ports are discovered, never GoFr's defaults.** GoFr refuses to start when a port is taken
and sbx is multi-instance by design — one daemon per branch, per worktree, per CI job — so
fixed `8000/2121/9000` would mean the second console on a machine dies on boot. Three consoles
run side by side; the chosen ports are printed at startup.

**It cannot slow anything down.** The daemon does not know this exists. It already writes a
structured JSON line for every wake and sleep, and this reads that stream — no callback, no
shared state, no socket between them, so there is no mechanism by which observability could
reach the byte-splice path.

| | |
|---|---|
| `/metrics` | `sbx_wakes_total`, `sbx_sleeps_total`, `sbx_wake_failures_total`, `sbx_wake_duration_ms` — all labelled by sandbox and service |
| `/.well-known/health`, `/alive` | auth-exempt, so Kubernetes probes need no credentials |
| `/api/sandboxes` | what the daemon is fronting, read-only |

Telemetry is off: `GOFR_TELEMETRY=false` is set before the app starts. A tool whose job is
watching your infrastructure should not be the thing making an outbound call you did not ask
for, and it has to work on a machine with no network at all.
