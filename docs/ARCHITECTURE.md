# Architecture

Everything here follows from one rule:

> **Nothing may start or stop a sandbox except the thing that can see demand.**

Anything else that can start one eventually leaves one running — then two components each
believe they own the lifecycle, and disagree while you debug something else.

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
            │       └──┬─────────┬──────────┬──┘       │
            │          │         │          │          │
            │     ┌────▼───┐ ┌───▼──────┐ ┌─▼─────────┐│
            │     │ docker │ │kubernetes│ │firecracker││
            │     └────────┘ └──────────┘ └───────────┘│
            │                                          │
   ┌────────▼────────┐                        ┌────────▼────────┐
   │   sbx serve     │                        │  tunnel backend │
   │  owns the ports │                        │  cloudflared /  │
   │  wakes & sleeps │                        │  ngrok / ssh*   │
   └─────────────────┘                        └─────────────────┘
                                               * opt-in only
```

One spec. One binary. Three backends.

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

| | docker | kubernetes | firecracker |
|---|---|---|---|
| address | `127.0.0.1:20002` | `sbx-x-pg.sbx.svc:5432` | `127.0.0.1:20002`, upstream the guest's tap IP |
| wake | `docker start` | scale → 1 | snapshot load + resume |
| sleep | `docker stop` | scale → 0 | snapshot (Diff) + kill the VMM |
| health | HEALTHCHECK | readinessProbe | first port accepts (declared=false until the guest agent runs checks) |
| storage | named volume | PVC | the VM's own ext4 root, cloned per VM |
| isolation | `--runtime` | `runtimeClassName` | a guest kernel, always |

The right-hand column is why the provider is an interface, not a flag: the wake policy above
doesn't know which it drives.

---

## MicroVM: firecracker

`--provider firecracker` makes each service a Firecracker VM whose sleeping state is a snapshot
on disk, so a wake brings memory and running processes back rather than a cold process against a
warm disk (ROADMAP §1). Linux with `/dev/kvm` only; everywhere else the path is decided by
`internal/fc/hostcap`, which `sbx doctor`, the provider and the helper-VM layer all share.

```
  hostcap.Probe → Decide ─┬─ direct ─────────── fcProvider (this machine)
                          ├─ helper-vm ──────── HelperVMProvider hook (macOS, Windows)
                          ├─ kata-runtimeclass ─ --provider kubernetes --isolation kata
                          └─ refused ────────── reason + the one thing to change

  <state>/fc/                                  SBX_FC_STATE, default ~/.sbx/fc
    artifacts/firecracker-v1.17.0-<arch>-<sha>/  pinned by sha256, .built + atomic rename
    artifacts/vmlinux-6.18.48-<arch>-<sha>/
    rootfs/<image id>/rootfs.ext4              docker export → mkfs.ext4 -d, keyed by image ID
    vms/<hash of ref>/                         0700, one per service; the socket path fits 108 bytes
      vm.json  api.sock  vsock.sock  console.log  vmm.log  firecracker.pid  lock
      agent.ext4 (vda, ro: /sbx + /init.json)  rootfs.ext4 (vdb, rw, reflink or sparse copy)
      vm.state  vm.mem                         the asleep state
    snapshots/<name>/                          sbx snapshot: memory + both drives
```

| verb | what happens |
|---|---|
| Create | build/reuse the rootfs, clone it, write the agent drive, cold boot, wait for the first port, **Seal**, Pause, Full snapshot, kill |
| Start | mark the snapshot invalid (fsync'd), load with `resume_vm`, `vsock_override` and `network_overrides`, **Rekey** before returning |
| Stop | **Seal**, Pause, Diff snapshot if this process was restored (else Full), kill, fold the Diff into `vm.mem` by extent, mark valid |
| a VM that died awake | its snapshot is invalid, so Start cold-boots against the disk instead of restoring stale memory over it |

The guest is PID 1 `sbx fc-init` on the agent drive: it mounts the image root, gives it proc, sys, dev,
devpts and the agent at `/opt/sbx/sbx`, switches root and execs `sbx execd --vsock-port 44772 -- <entrypoint>`.
The guest's address comes from the kernel command line (`ip=`, `CONFIG_IP_PNP=y` in the pinned kernel).

**The guest seam.** Everything the lifecycle needs from inside the VM is `fc.Guest` - `Dial`, `Seal`,
`Rekey`, `Available` - assigned through `fc.NewGuest`. The shipped `fc.NoGuest` refuses all four; with
it the provider sleeps and wakes (same identity, nothing to re-key) and refuses exec, copy and
forking. The vsock work (`internal/fcvsock`, `internal/execdctl`) implements it; the daemon's
`GuestDialer` adapts `fcProvider.DialGuestPort`. Until then the wake proxy dials `Upstream` - the
guest's tap address - over TCP, which a direct Linux host can reach.

Nothing in the provider assumes it is the process that started a VM or the one facing the user:
every fact is in `vm.json` or answered by the API socket, and every operation takes the VM's
flock. That is what lets `sbx create` boot a VM that `sbx serve` later sleeps, and what lets the same
code run inside the helper VM.

---

## What is deliberately not here

| | why |
|---|---|
| `sbx start` / `sbx stop` | the rule at the top |
| A tunnel implementation | Cloudflare delegates theirs too; we shell out |
| Preview URLs in cluster mode | that is an Ingress, and it already exists |
| Code interpreters | a language runtime product, not a sandbox one |
| Multi-tenant hardening | `--isolation gvisor\|kata` is declarable; operating it is yours |
