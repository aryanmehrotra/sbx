# OpenSandbox compatibility — design

> **Short version:** `sbx serve --osb-addr 127.0.0.1:8080` speaks OpenSandbox's lifecycle API, and
> every sandbox created through it runs `sbx execd` inside, speaking OpenSandbox's execd API. The
> definition of done is not a feature table: it is **OpenSandbox's own Go e2e suite
> (`tests/go`, pinned at `release-1.1.0`) passing unmodified against sbx**, plus sbx's own tests.

Pinned upstream: `github.com/alibaba/OpenSandbox` tag `release-1.1.0` (2026-09-21). Specs:
`specs/sandbox-lifecycle.yml` (2383 lines), `specs/execd-api.yaml` (2631), `specs/egress-api.yaml`
(708), `specs/diagnostic-api.yml` (358). Every claim below about upstream behaviour is checked
against that tag, not against `main` and not against docs.

## Goal

Any OpenSandbox client — the Go/Python/JS/Kotlin/C# SDKs, the `osb` CLI, the MCP server — pointed
at `sbx serve` works, on a laptop, with no FastAPI server, no database and no Kubernetes. And it
keeps what only sbx has: a sandbox nobody is talking to costs nothing, and the next request wakes
it.

## What stays true (DECISIONS.md)

- **Zero dependencies in the root `go.mod`.** The conformance suite lives in its own module under
  `test/osb/` and is the only place testify and the upstream SDK are allowed.
- **No identity system.** OpenSandbox authenticates with one key header (`OPEN-SANDBOX-API-KEY`).
  sbx accepts exactly **one operator key** — "is this yours", the same posture as
  `--connect-addr`'s token — never per-user keys or tenants. This is recorded as an amendment to
  *sbx is a tool people run, not a service anyone offers*: the lifecycle API is local by default
  (loopback bind), and binding it elsewhere requires `--osb-key` and prints the same warning the
  connect endpoint does. Multi-tenancy from upstream (key → namespace) is **out of scope**.
- **Capabilities are negotiated, not stubbed.** An endpoint whose capability the provider lacks
  answers with OpenSandbox's own error shape (`{code,message}`) and a status the SDK maps to
  "not supported" — never a 200 that did nothing.

## Shape

```
 OpenSandbox SDK / osb / MCP
        │  OPEN-SANDBOX-API-KEY
        ▼
 sbx serve ──── internal/osb (lifecycle API, /v1/...)
   │   state: <state dir>/osb/<id>.json  (expiresAt, metadata, status, reason)
   │   reaper: expiry → rm ; idle → pause (default) or sleep (opt-in)
   │
   ├── provider (docker | kubernetes)         ← unchanged interface + Pauser capability
   │
   └── wake proxy ── 127.0.0.1:<slot> ──► container :44772  sbx execd  (internal/execd)
                                             │ commands · sessions · files · dirs
                                             │ metrics · code (Jupyter) · pty · /proxy/{port}
                                             └ exec's the user entrypoint as its child
```

### Sandboxes created through the API

- One sbx sandbox per OpenSandbox sandbox, **name = id** (`osb-` + 12 hex). It holds one service,
  `sandbox`, whose image is the request's image. `sbx list` shows them like any other.
- **execd injection.** The linux `sbx` binary for this version is placed in a named volume
  `sbx-execd-<version>` (source, in order: `$SBX_EXECD_BINARY`; the host binary itself when the
  host is linux/same-arch; a cross-compile from the module cache when `go` is present; the
  published `ghcr.io/aryanmehrotra/sbx-activator:<version>` image). It is mounted read-only at
  `/opt/sbx` and the entrypoint becomes `/opt/sbx/sbx execd -- <entrypoint...>`. execd
  forwards signals to and reaps its child, and exits with the child's code — so the user's
  entrypoint behaves as PID 1 would.
- **execd port 44772** is the service's declared port and goes through the wake proxy. The access
  token is minted per sandbox and returned in `endpoints/44772` as `X-EXECD-ACCESS-TOKEN`.
- **Any other port** resolves to `<execd endpoint>/proxy/<port>` (HTTP and WebSocket), because a
  slot is allocated at create time and OpenSandbox lets a client ask for a port nobody declared.
  Ports listed in the request's `extensions["sbx.ports"]` get real slots (raw TCP, wakes on
  connect) instead.

### Lifecycle mapping

| OpenSandbox | sbx |
|---|---|
| `POST /sandboxes` → `Pending` | create returns immediately; status goes `Running` once execd answers `/ping` |
| `timeout` (s, ≥60, null = never) | `expiresAt` in the state file, survives daemon restarts; reaper removes |
| `renew-expiration` | must be later than current, else 400 `INVALID_EXPIRATION` |
| `pause` / `resume` | **docker pause/unpause (cgroup freezer): memory and processes kept**; k8s: scale to 0 (filesystem only), reported honestly in `status.message` |
| idle | default: freeze after `idle` (5m); a request to execd thaws it (~10 ms). `extensions["sbx.idle"]="sleep"` stops it to 0 B instead |
| `DELETE` | `Stopping` → rm → `Terminated` |
| `metadata` PATCH | stored in state file and as labels |
| `endpoints/{port}` | above |
| `snapshots` | `docker commit` of the sandbox container to `sbx-osb-snap:<id>` (paused by docker for the copy); `snapshotId` on create = fork: new id, new token, no pull, default entrypoint `tail -f /dev/null`. Volumes are not in it. DECISIONS: "An API snapshot is the container" |
| `templates` | an image or a snapshot id + entrypoint/cpu/memory; "building" = pull + inspect here; `templateId` needs `timeout`. `readiness` and `resourceLimits.disk` refused; `publish`/`format` echoed, unused |
| `networkpolicy` | the egress filter, **live-updatable**, FQDN + wildcard + CIDR |
| `diagnostics/logs|events` | provider logs + sbx history events, plain text |
| `metrics/events` | accepted and recorded in history |
| volumes `host` / `pvc` | bind mount only under `--osb-host-paths` roots (none by default; symlinks resolved) / docker volume `sbx-osb-pvc-<claimName>` - namespaced, unlike upstream. `createIfNotExists`, `deleteOnSandboxTermination` (only for a volume this create made), `subPath`, `readOnly`; storage hints ignored as the spec says. Kubernetes: 501 |
| `volumes.ossfs` | refused: 400 with a message naming the backend (Alibaba OSS, out of scope) |
| `resourceLimits` | existing cpu/memory limits |
| `platform` | image platform on pull; mismatch refused |

### execd

A stdlib-only HTTP server in `internal/execd`, run as `sbx execd`. Endpoints follow
`execd-api.yaml` exactly (paths, JSON field names, SSE event shapes, status codes), and where the
spec is silent the upstream execd at the pinned tag is the reference for behaviour, not for code.
Groups: ping · command (foreground SSE, background + status + logs, interrupt) · bash session ·
files (info/delete/permissions/mv/search/replace/upload/download with range) · directories ·
metrics + watch · code contexts (Jupyter kernel protocol over a stdlib WebSocket client, when the
image has a Jupyter server; otherwise 501 with the reason) · pty (WebSocket) · isolated sessions
(bubblewrap + overlay when present; `capabilities` reports what is not) · `/proxy/{port}`.

### Beyond upstream (where sbx should win)

- **Wake on request** — an idle sandbox costs 0 CPU (frozen) or 0 B (asleep), and the SDK never
  sees a refused connection.
- **Pause keeps memory** on docker, which upstream's docker runtime does not.
- **One binary, no server stack**: `go install` + docker is the whole deployment.

## Out of scope

Multi-tenancy, per-user keys, quotas; Fast Sandbox / Firecracker templates (ROADMAP §1);
ossfs; hosting; the Kubernetes CRDs (BatchSandbox/Pool) — the **SDK-side pool** works because it
is client-side. A server-side warm pool is in scope (below) because it is what makes create fast.

## Releases (each one shippable, each one green on the conformance subset it claims)

| tag | adds | conformance files that must pass |
|---|---|---|
| v0.9.0 | lifecycle core, execd core, expiry/renew, pause/resume, endpoints, metadata, diagnostics | `sandbox`, `command`, `filesystem`, `manager`, `lifecycle_metrics`, `error_handling`, `concurrent`, `streaming_timeout` |
| v0.10.0 | snapshots, templates, networkpolicy + live egress + CIDR, volumes, code interpreter, pty, `/proxy`, **warm pool behind create** | + `volume`, `egress_env`, `code_interpreter` (`RUN_CODE_INTERPRETER_E2E=true`) |
| v0.11.0 | isolated sessions, credential vault (TLS-terminating egress), server proxy mode, secure access, renew-on-access | + `isolated_session`, `credential_vault`, `pool` |
| v0.12.0 | `sbx mcp` (stdio), public Go package, `@computesdk/sbx` provider passing their `test-utils` suite, their Burst-TTI harness run locally | everything not needing an LLM endpoint |

## Testing

1. **Unit** per package, stdlib only, with the test written to fail first.
2. **`scripts/osb-conformance.sh [pattern]`** — starts a throwaway `sbx serve --osb-addr` on a free
   port with a random key, runs `test/osb` (which vendors nothing: `go.mod` pins
   `github.com/alibaba/OpenSandbox/sdks/sandbox/go` and `tests/go` at the tag via a `replace` to a
   fetched checkout), and reports pass/skip/fail per upstream test. A skip is reported by name;
   **a skip is not a pass**, and the release table above lists what may be skipped and why.
3. **CI** gets an `osb-conformance` job next to `e2e`.
4. **Bench**: `scripts/osb-bench.sh` — create→first command, command round trip, wake from
   frozen, wake from asleep; interleaved and alternated per CONTRIBUTING.

## Must not regress

The wake path and its benchmarks; every existing command's behaviour on sandboxes not created
through the API; zero deps; `-race` clean; all existing e2e scripts.

## Risks accepted

- execd injection needs a linux binary of the *same* version; dev builds use the cross-compile
  path, and a machine with neither Go nor network cannot create API sandboxes (refused with that
  reason).
- Freezing instead of stopping on idle holds memory. It is the default only for API sandboxes,
  whose contract (running processes survive) needs it.

## Speed target (ComputeSDK Burst TTI)

ComputeSDK ranks 38 hosted providers on **TTI = `create()` → first successful `runCommand('node -v')`,
100 sandboxes launched at once**, from a CI VM over the internet. Leader on 2026-09-25: isorun,
**44 ms median / 49 ms p95**; e2b 1238 ms; daytona 341 ms
(`computesdk/benchmarks` `results/burst_tti/latest.json`).

Every sandbox in that test is new, so wake-on-connect does not help and `docker run` (hundreds of
ms) cannot win. What can: a **warm pool** — N containers per (image, limits) already created,
execd answering, then frozen. `create()` claims one, rewrites its identity (id, token, env) through
execd, thaws it and returns `Running`; the pool refills behind it. Target, measured locally with
their harness against `sbx serve`: **median < 44 ms at burst 100**. A local result is not a
leaderboard entry — their runner needs a reachable hosted endpoint, which is the operator's
decision, not this design's.

## The goal, as the operator set it (2026-09-25)

1. **All of OpenSandbox's specification, end to end** — lifecycle, execd, egress/credential
   vault, diagnostics — proven by upstream's own `tests/go`, not by a feature table.
2. **Functionality and use cases first; optimise wherever it is cheap to.**
3. **Go only.**
4. **Released**: pushed to `aryanmehrotra/sbx` and tagged, in successive versions (up to five).
5. **Hosted**, and listed on ComputeSDK with the aim of **#1 on Burst TTI** (current leader
   44 ms median) and the widest feature/use-case coverage of any listed provider.

Hosting needs a machine where sbx can reach a container runtime (a docker socket or a Kubernetes
API). ZopCloud's shared VM pool runs services as containers with no documented way to mount one;
that, and creating the project there, are the operator's call.

## Amendments (2026-09-26)

Where the build departed from the text above. The text is left as written so the departure is
visible; this section is what is true.

### Provider capabilities actually added

"Unchanged interface + Pauser capability" undercounted. The core `Provider` interface is still
unchanged; the optional capabilities the API needed are:

| capability | what it is for | kubernetes |
|---|---|---|
| `Pauser` | `pause`/`resume` and freeze-on-idle: `docker pause`, memory kept | not implemented |
| `Injector` | put execd into an arbitrary image: seed a named volume once, mount it read-only at `/opt/sbx`, read the image's entrypoint and platform | not implemented (needs an init container) |
| `NamedVolumes` | `pvc` volumes as namespaced docker volumes (`sbx-osb-pvc-<claim>`) | not implemented (a PVC's class and size are the operator's) |
| `Snapshotter.Commit(ctx, ref, image, changes...)` | gained `changes` (`docker commit --change`), so a snapshot image does not carry the source sandbox's execd token in its `ENV` | n/a (no Snapshotter) |
| `EgressPreflighter` | ask once, before anything is created, whether this host can enforce an egress policy, so a refusal leaves no half-made sandbox | n/a |
| `Puller` | already existed (prewarm); the API pulls through it | refuses |

### Kubernetes: the API is docker-only until an Injector exists

The lifecycle table's "k8s: scale to 0 (filesystem only), reported honestly" never shipped. Every
API sandbox needs execd inside an image sbx did not build, and the kubernetes provider has no
`Injector`, so `POST /v1/sandboxes` answers 501 `SANDBOX::API_NOT_SUPPORTED` naming the reason.
`pause` would be refused by name in any case: a scale-to-zero discards the memory, and calling it a
pause is the stub the capability pattern exists to avoid. Kubernetes becomes an API backend when an
init-container `Injector` is built; `sandbox.json` sandboxes on a cluster are unaffected.

### Metadata is not written as labels

"Stored in state file and as labels" is only the first half. Docker labels are fixed when a
container is created, so a label copy would be stale after the first `PATCH` - and recreating a
container to relabel it is a restart the caller did not ask for. Metadata, expiry, the token, the
held-pause flag and the pvc volumes a sandbox owns live in `~/.sbx/osb/<id>.json` only. Labels carry
what docker's own view needs (sandbox, service, slot, ports, idle policy) and nothing the API edits.

### Built beyond the Shape

| component | why it exists |
|---|---|
| egress control (`internal/egress`, the filter container, live policy push) | `networkpolicy` with FQDN, wildcard and CIDR rules, updatable without recreating the container; deny rules are real because the bridge has no NAT |
| spec fields `entrypoint`, `cap_add`, `on_idle`, `egress_policy` | what an API sandbox needs, expressed as ordinary provider-neutral spec rather than API-only plumbing. `readonly_volumes` and `volume_mounts` started as spec fields and are now in-memory only (`json:"-"`): as sandbox.json fields they let a spec mount any named volume, sidestepping the API's namespacing |
| `internal/jupyter`, `internal/wsclient`, `internal/wsserver` | execd's `/code` (the Jupyter kernel protocol over WebSocket) and `/pty`, standard library only |
| `internal/mcp`, `internal/osbclient` | an MCP server over stdio for agents, speaking the OpenSandbox contract through a small client of its own |
| `sbx serve --only` (`internal/daemon/scope.go`) | fences a second daemon to a prefix (`--only osb-`), so a conformance run beside a live stack never fronts, sleeps or reaps it |
| `internal/slotlock` | a machine-wide lock around slot choice; concurrent creates otherwise picked the same slot and three of four failed at `docker run` |
| health start interval | API sandboxes check health every 60s with a 1s start interval, not every 5s - 12x fewer runc execs (DECISIONS.md) |
