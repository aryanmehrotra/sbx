# Architecture

Everything here follows from one rule:

> **Nothing may start or stop a sandbox except the thing that can see demand.**

Whatever else can start one eventually becomes the thing that left one running, and then two
components believe they own the lifecycle and disagree while you are debugging something
else.

---

## The pieces

```
                    ┌──────────────────────────┐
                    │      sandbox.json        │  ← the only thing a repo commits
                    │  services · health · env │
                    └────────────┬─────────────┘
                                 │ read by
                    ┌────────────▼─────────────┐
            ┌───────┤        sbx (CLI)         ├───────┐
            │       │ create env ready exec    │       │
            │       │ logs cp url list rm      │       │
            │       └────────────┬─────────────┘       │
            │                    │                     │
            │       ┌────────────▼─────────────┐       │
            │       │    Provider interface    │       │
            │       │  Create Start Stop Probe │       │
            │       │  List Exec Logs Copy     │       │
            │       └──────┬────────────┬──────┘       │
            │              │            │              │
            │       ┌──────▼─────┐ ┌────▼────────┐     │
            │       │   docker   │ │ kubernetes  │     │
            │       └────────────┘ └─────────────┘     │
            │                                          │
   ┌────────▼────────┐                        ┌────────▼────────┐
   │   sbx serve     │                        │  tunnel backend │
   │  owns the ports │                        │  cloudflared /  │
   │  wakes & sleeps │                        │  ngrok / ssh*   │
   └─────────────────┘                        └─────────────────┘
                                               * opt-in only
```

One spec. One binary. Two backends.

---

## Local: the two-port trick

The problem: something has to answer while nothing is running.

```
   your client                              sbx serve
   (psql, redis-cli, a pool,             ┌──────────────┐
    Playwright, curl)                    │  always up   │
        │                                │   ~4.5 MB    │
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

Because it splices bytes rather than parsing a protocol, it works for Postgres, Redis, gRPC
and anything else over TCP. The RBAC it runs under can **scale** a Deployment and cannot
**create or destroy** one.

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

**Bytes, not connections.** A connection pool holds sockets open forever; a sandbox fronted
by a running service would never sleep.

**The guard is not theoretical.** Without it, the activator scaled a sandbox to zero 39
seconds into its own creation, while the command creating it was still waiting.

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
        slot 1 ──▶ public  20020…
```

**Allocated, not hashed.** Hashing names into 60 slots collided on the first six branch
names tried, and two sandboxes on one slot fight over ports. Docker labels are the registry,
so there is no state file to drift from reality.

**Optional services still reserve ordinals**, so adding one later never moves an existing
service out from under a config that recorded where it was.

In a cluster none of this applies: a pod has its own address, so Postgres is `:5432` on a
name. The port arithmetic is a workaround for one shared loopback.

---

## The same spec, either backend

```sh
sbx create my-branch                        # docker, this machine
sbx create my-branch --provider kubernetes  # the same spec, a cluster
```

Everything the spec declares maps onto both. Nothing in `sandbox.json` names a backend:

| | docker | kubernetes |
|---|---|---|
| address | `127.0.0.1:20002` | `sbx-x-pg.sbx.svc:5432` |
| wake | `docker start` | scale → 1 |
| sleep | `docker stop` | scale → 0 |
| health | HEALTHCHECK | readinessProbe |
| storage | named volume | PVC |
| isolation | `--runtime` | `runtimeClassName` |

The right-hand column is the reason the provider is an interface rather than a flag: the wake
policy above doesn't know which of these it is driving.

---

## What is deliberately not here

| | why |
|---|---|
| `sbx start` / `sbx stop` | the rule at the top |
| A tunnel implementation | Cloudflare delegates theirs too; we shell out |
| Preview URLs in cluster mode | that is an Ingress, and it already exists |
| Code interpreters | a language runtime product, not a sandbox one |
| Multi-tenant hardening | `--isolation gvisor|kata` is declarable; operating it is yours |
