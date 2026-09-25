# Architecture

Everything here follows from one rule:

> **Nothing may start or stop a sandbox except the thing that can see demand.**

Anything else that can start one eventually leaves one running — then two components each
believe they own the lifecycle, and disagree while you debug something else.

### The exceptions, named

The OpenSandbox API is a lifecycle API, so it cannot keep the rule whole. Each place it bends is
listed here, and each is either an explicit request from the caller or bounded by the daemon:

| what starts or stops a sandbox | why it is allowed | who then owns it |
|---|---|---|
| API `pause` | the caller asked for a freeze that traffic must not undo; the daemon **holds** it | the hold, until `resume` |
| API `resume` | the caller asked; it releases the hold and thaws | the daemon again |
| API `DELETE`, and the expiry reaper | the sandbox's `timeout` is the caller's; removal is the end of it, not a start | nobody — it is gone |
| API `POST .../snapshots` | `docker commit` pauses a running container for the copy (crash-consistent) and thaws it | the daemon; a stopped one is committed without being started |
| warm-pool members | created running ahead of any caller and **pinned**: the reaper leaves them alone until a claim | the daemon, from the claim on — pinning ends and the idle clock starts then |
| `create` (CLI or API) | a new container is started once, to be made; asking is starting | the daemon, from its first idle check |

Anything not in this table that starts or stops a container is a bug against the rule.

---

## The pieces

```
                    ┌──────────────────────────┐   ┌──────────────────────────┐
                    │      sandbox.json        │   │  OpenSandbox SDK / HTTP  │
                    │  services · health · env │   │  POST /v1/sandboxes ...  │
                    └────────────┬─────────────┘   └────────────┬─────────────┘
                                 │ read by                      │ served by
                    ┌────────────▼─────────────┐   ┌────────────▼─────────────┐
            ┌───────┤        sbx (CLI)         │   │  osb: lifecycle API      │
            │       │ create env ready exec    │   │  (in sbx serve)          │
            │       │ logs cp url list rm      │   │  records · expiry ·      │
            │       └────────────┬─────────────┘   │  warm pool · volumes     │
            │                    │                 └──┬──────────────────┬────┘
            │                    │ ┌──────────────────┘                  │
            │       ┌────────────▼─▼───────────┐                         │ Hold · Pin
            │       │    Provider interface    │                         │ Freeze · Thaw
            │       │  Create Start Stop Probe │                         │ Refresh
            │       │  List Exec Logs Copy     │                         │
            │       │  + optional: Injector    │                         │
            │       │  Pauser Snapshotter      │                         │
            │       │  NamedVolumes Puller ... │                         │
            │       └──────┬────────────┬──────┘                         │
            │              │            │                                │
            │       ┌──────▼─────┐ ┌────▼────────┐                       │
            │       │   docker   │ │ kubernetes  │  ← no Injector:       │
            │       └────────────┘ └─────────────┘    the API is 501     │
            │                                                            │
   ┌────────▼────────┐                                                   │
   │   sbx serve     │◀──────────────────────────────────────────────────┘
   │  owns the ports │
   │  wakes & sleeps │        tunnel backend (cloudflared / ngrok / ssh):
   │  freezes idle   │        opt-in only, shelled out to by `sbx url`
   │  API sandboxes  │
   └─────────────────┘
```

One spec. One binary. Two backends — and one API in front of the first of them.

---

## Local: the two-port trick

The problem: something has to answer while nothing is running.

```
   your client                              sbx serve
   (psql, redis-cli, a pool,             ┌──────────────┐
    Playwright, curl)                    │  always up   │
        │                                │   ~9.1 MB    │
        │  :20002  ── PUBLIC ────────────▶              │
        │            (owned by sbx)      └──────┬───────┘
        │                                       │
        │                        ┌──────────────▼──────────────┐
        │                        │ serving?  (Probe, not the   │
        │                        │           platform's guess) │
        │                        └───┬──────────────────┬──────┘
        │                        no  │                  │ yes
        │                     ┌──────▼──────┐           │
        │                     │ docker start│           │
        │                     │   ~110 ms   │           │
        │                     └──────┬──────┘           │
        │                            └────────┬─────────┘
        │                                     │
        │                          :30002 ── BACKING ──▶ ┌─────────┐
        │                          (docker publishes;    │ postgres│
        │                           gone while asleep)   │  :5432  │
        └◀────────────── bytes spliced ──────────────────┴─────────┘
```

---

## Cluster: the activator

A Service that selects the workload **cannot answer at zero replicas**. So there are two.

```
   ASLEEP                                         Deployment
   ────────────────────────────────────           replicas: 0
                                                       ▲
   client ──▶ sbx-x-pg:5432 ─────▶ ┌──────────────┐    │ scale 1
              (client Service,     │  activator   │────┘
               selects ACTIVATOR)  │  (sbx serve) │
                                   └──────┬───────┘
                                          │ then dials
                                          ▼
                                 sbx-x-pg-app:5432
                              (workload Service, selects PODS)
                                          │
                                     ┌────▼────┐
                                     │  pod    │  ← PVC survives sleep
                                     └─────────┘
```

It splices bytes rather than parsing a protocol, so it works for Postgres, Redis, gRPC —
anything over TCP. Its RBAC can **scale** a Deployment, not **create or destroy** one.

---

## Lifecycle

```
                  a connection / exec / URL hit
        ┌──────────────────────────────────────────┐
        │                                          ▼
   ┌────┴─────┐                              ┌──────────┐
   │  ASLEEP  │                              │  AWAKE   │
   │   0 B    │◀─────────────────────────────│          │
   └──────────┘   no bytes for --idle        └──────────┘
        │              (reaped every idle/3)       │
        │                                          │
        └──────── volume / PVC persists ───────────┘

   guard: cannot sleep until seen serving once
```

**Bytes, not connections.** A pool holds sockets open forever, so a sandbox fronted by a
running service would never sleep.

**The guard is not theoretical.** Without it, the activator scaled a sandbox to zero 39
seconds into its own creation, while the creating command was still waiting.

---

## Addressing

```
   sandbox.json services (alphabetical, stable)
   ├── clickhouse :9000 :8123 ──▶ ordinal 0, 1
   ├── postgres   :5432       ──▶ ordinal 2
   └── redis      :6379       ──▶ ordinal 3
                                      │
              slot allocated from labels (not hashed)
                                      │
        slot 0 ──▶ public  20000 + 0×20 + ordinal
                   backing 30000 + 0×20 + ordinal
        slot 1 ──▶ public  20020...
```

**Allocated, not hashed.** Hashing names into 60 slots collided on the first six branch
names tried, and two sandboxes on one slot fight over ports. Docker labels are the registry —
no state file to drift from reality.

**Optional services still reserve ordinals**, so adding one later never shifts an existing
service out from under a config that recorded its port.

None of this applies in a cluster: a pod has its own address, so Postgres is `:5432` on a
name. The port arithmetic is a workaround for one shared loopback.

---

## The same spec, either backend

```sh
sbx create my-branch                        # docker, this machine
sbx create my-branch --provider kubernetes  # the same spec, a cluster
```

Everything the spec declares maps onto both; nothing in `sandbox.json` names a backend:

| | docker | kubernetes |
|---|---|---|
| address | `127.0.0.1:20002` | `sbx-x-pg.sbx.svc:5432` |
| wake | `docker start` | scale → 1 |
| sleep | `docker stop` | scale → 0 |
| health | HEALTHCHECK | readinessProbe |
| storage | named volume | PVC |
| isolation | `--runtime` | `runtimeClassName` |

The right-hand column is why the provider is an interface, not a flag: the wake policy above
doesn't know which it drives.

---

## The OpenSandbox API

`sbx serve --osb-addr` also answers OpenSandbox's lifecycle API. An API sandbox is an ordinary sbx
sandbox named by its id (`osb-` + 12 hex) with one service, and the daemon wakes and idles it like
any other. What the API adds is around it, not instead of it:

```
   SDK ──▶ osb (sbx serve)                         one API sandbox
           │                                      ┌──────────────────────────────────┐
           │ create: pull · place execd ─────────▶│ /opt/sbx  ← execd volume, ro     │
           │         (Injector) · docker run      │ sbx execd --addr :44772 -- <cmd> │
           │                                      │   /command /files /code /pty     │
           │ records: ~/.sbx/osb/<id>.json        │   /proxy  (execd is PID 1)       │
           │   token · expiry · metadata ·        │ <cmd>  ← the caller's entrypoint │
           │   owned pvc volumes                  └───────────────▲──────────────────┘
           │                                                      │
           │ pause ──▶ Freeze + HOLD  (traffic cannot thaw it)    │ wake port, fronted
           │ resume ─▶ Thaw  (hold released)                      │ by the daemon
           │                                                      │
           └────────────────────────▶ daemon ─────────────────────┘
                                        idle: FREEZE (docker pause, memory kept,
                                              ~10 ms thaw on the next byte) — the
                                              API default; extensions["sbx.idle"]=
                                              "sleep" stops it to 0 B instead
                                        pool: members wait running and PINNED,
                                              re-keyed on claim, then idle as usual
```

- **execd is injected, not baked.** The binary is copied once into a named volume and mounted
  read-only at `/opt/sbx` in any image the caller names — `Injector`, which only docker
  implements. A cluster would need an init container, so the API answers 501 there rather than
  pretending (`pause` would be a scale-to-zero that loses the memory, and is refused by name).
- **A held pause is not an idle freeze.** Both are `docker pause`. The idle one is the daemon's and
  the next byte undoes it; the held one is the caller's, reported as `Paused`, and refuses traffic
  until `resume`. The hold is re-asserted from the record when the daemon restarts.
- **Freeze-on-idle is the API default** because upstream's contract is that a background process
  started in one request is still running at the next; `sandbox.json` keeps stop-on-idle, since
  holding memory is the opposite of why sbx exists.
- **What the API remembers lives in its record file**, never in labels: docker labels are fixed at
  create, and metadata, expiry and holds change.

---

## What is deliberately not here


| | why |
|---|---|
| `sbx start` / `sbx stop` | the rule at the top |
| A tunnel implementation | Cloudflare delegates theirs too; we shell out |
| Preview URLs in cluster mode | that is an Ingress, and it already exists |
| A code-interpreter runtime | execd's `/code` drives the Jupyter an image already runs (`opensandbox/code-interpreter`); sbx ships no kernels |
| Multi-tenant hardening | `--isolation gvisor\|kata` is declarable; operating it is yours |
