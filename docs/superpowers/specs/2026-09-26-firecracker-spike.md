# Spike: a Firecracker provider, on this Mac and on Linux

> **Question.** Can sbx drive Firecracker on macOS (ROADMAP §1 option B, nested virtualisation)
> and on Linux, and what does a snapshot-restore wake cost - including at burst, which is what
> the ComputeSDK Burst-TTI target (leader 44 ms median create → first command) needs?
>
> **Answer.** It **works** on macOS through nested virtualisation, unmodified, driven by a
> stdlib-only Go client. It is **not fast** there: snapshot load is 5 ms, but the first byte
> after resume is 88 ms median, a cold boot is 1.5 s, and concurrent restores collapse
> superlinearly (10 at once: 7 s; 50 at once: most never answer). Every one of those costs is
> nested-virtualisation exit overhead, not Firecracker. Nothing here measured bare-metal Linux,
> which is the only place the ROADMAP's 4–28 ms can be confirmed or refuted.

## Setup

| | |
|---|---|
| Host | Apple M4, macOS 26.4.1, 16 GiB (≈4 GiB free, swap in use, two other colima VMs running) |
| L1 VM | colima 0.10.1 profile `fc`, `--vm-type vz --nested-virtualization --cpu 2 --memory 2 --disk 20`, Ubuntu 24.04.4, kernel 6.8.0-100-generic aarch64 |
| KVM | `/dev/kvm` present (`crw-rw---- root kvm 10,232`). Go ioctl probe: `KVM_GET_API_VERSION`=12, `KVM_CAP_ARM_VM_IPA_SIZE`=40, `KVM_CAP_MAX_VCPUS`=512, `KVM_CAP_ARM_PMU_V3`=0, `KVM_CREATE_VM` succeeds |
| VMM | Firecracker **v1.17.0** aarch64, `firecracker-v1.17.0-aarch64.tgz` sha256 `e351ebe4f7a16b5873bbd51005d2e6767103cff4d5ebc829df2d3f95a93e2256` (matches the release's `.sha256.txt`) |
| Guest kernel | Firecracker CI `firecracker-ci/20260923-6f82ac4cf331-0/aarch64/vmlinux-6.18.48`, sha256 `a80108af80d9549b357ea7e00bd5c12f80686869541d135a8a67f6fe1ec3451e` |
| Reference rootfs | same prefix, `ubuntu-24.04.squashfs`, sha256 `59da3690f36162c27e9f0e9122e90675cac9af72d3cf56a76990715cc97b0dad` (downloaded, not used for timing - see below) |
| Guest under test | 32 MiB read-only ext4 holding one file, `/init`: a 1.8 MB static Go binary that mounts `/proc`, `/sys` and serves HTTP on `AF_VSOCK` port 5000. It stands in for **execd as PID 1**. 1 vCPU, 128 MiB. `console=ttyS0 reboot=k panic=1 quiet loglevel=1 init=/init` |
| Driver | a stdlib-only Go program (`net/http` over a unix `DialContext`, `os/exec`), run as root in the L1 VM |

Artifact URLs follow the pinned `docs/getting-started.md` at `v1.17.0`: the CI bucket is
`https://s3.amazonaws.com/spec.ccfc.min`, keyed by a dated prefix, not by version. **The
`firecracker-ci/v1.16/` and `v1.17/` prefixes are empty**; the last versioned prefix is `v1.15`.
A pipeline that computes `firecracker-ci/v${major.minor}/` from the release tag - which older
guides do - finds nothing.

## Measurements

Single-VM rows: 30 rounds, cold boot and restore **interleaved and alternating order** each round
(CONTRIBUTING). "First byte" is the first byte of an HTTP response read through Firecracker's
hybrid vsock (connect to the UDS, `CONNECT 5000\n`, `OK`, `GET /`), timed from before
`exec` of the firecracker process.

| | n | median | p95 |
|---|---|---|---|
| cold: spawn → API socket answers | 30 | 1.56 ms | 3.74 ms |
| cold: + configure (boot-source, drive, machine-config, vsock) | 30 | 3.13 ms | 7.34 ms |
| cold: + `InstanceStart` returns | 30 | 14.66 ms | 43.31 ms |
| **cold: spawn → first byte** | 30 | **1.493 s** | 2.186 s |
| restore: spawn → API socket answers | 30 | 1.75 ms | 3.15 ms |
| restore: + `PUT /snapshot/load` (`resume_vm: true`) returns | 30 | **5.04 ms** | 9.64 ms |
| **restore: spawn → first byte** | 30 | **87.96 ms** | 124.61 ms |
| vsock connect (dial + `CONNECT` + `OK`) on a restored VM | 150 | 4.13 ms | 12.73 ms |
| pause → resume → first byte, VM already faulted in | 10 | **3.08 ms** | 21.31 ms |
| restored VM, 2nd full request | 10 | 24.53 ms | 40.96 ms |
| restored VM, 3rd full request | 10 | 13.97 ms | 18.68 ms |

Concurrent restores from one snapshot (N goroutines released together; each its own firecracker
process, its own directory, `vsock_override` per clone, the same `vm.mem` mapped by all):

| N | spawn → load returned, median | spawn → first byte, median / p95 | all ready | answered |
|---|---|---|---|---|
| 2 | - | 198 ms / 204 ms | 204 ms | 2/2 |
| 5 | - | 1.63 s / 1.79 s | 1.80 s | 5/5 |
| 10 | 148 ms | 7.11 s / 7.35 s | 7.50 s | 10/10 |
| 20 | - | 23.4 s / 27.4 s | 27.6 s | 20/20 |
| 50 | - | 1 m 56 s (the 8 that answered) | - | **8/50** within 90 s |

Sequential restores with the earlier VMs left running: #1 252 ms, #10 518 ms, #20 593 ms,
#30 1.42 s (median over 30: 611 ms). **An idle restored VM costs 3.5 % of an L1 core** (30 idle
VMs: 1.05 CPU-s per second of 2), from `CONFIG_HZ=100` ticks each paying a nested exit.

Snapshot sizes (128 MiB guest):

| file | apparent | allocated | create time |
|---|---|---|---|
| `vm.mem` (Full) | 128 MiB | **128 MiB** | 227 ms (paused, fsync) |
| `vm.state` | 6,294 B | 8 KiB | (same call) |
| `diff.mem` (Diff, after resume + 20 requests + 1 s) | 128 MiB | **1.4–2.1 MiB** (sparse) | 30–36 ms |

Diff snapshots **work on aarch64** in v1.17.0 (`track_dirty_pages: true` in machine-config).
A Full `vm.mem` is written dense even though the guest touched a fraction of it; punching holes
afterwards (or keeping Full + Diff chains) is where the "a sleeping sandbox has a disk number"
cost in ROADMAP §1 gets small.

## Why restore is slow here, and why that is the Mac and not Firecracker

- **Load is fast, the guest's first touch is not.** `snapshot/load` returns in 5 ms. The first
  request then takes ~100 ms, and the firecracker process takes **~300 minor faults
  (≈1.2 MiB) during it** (`/proc/<pid>/stat` minflt before/after). That is ≈250–300 µs per
  fault: every stage-2 fault of the L2 guest is handled by L1 KVM, whose own hypervisor
  operations trap to macOS. On bare metal a stage-2 fault on a page-cache-hot MAP_PRIVATE file
  is expected to be single-digit µs, so the same 300 faults should be ~1–2 ms - an estimate, not measured here.
- **Once faulted in, a wake is ~3 ms** (pause → resume → first byte, 3.08 ms median) - still
  an order of magnitude above what a bare-metal vsock round trip is expected to cost (not measured here), because each virtio notification is a nested exit.
- **Concurrency collapses because the L1 has 2 vCPUs and every L2 exit is expensive.** At
  N=10 `snapshot/load` still returns in 148 ms median; it is the post-resume guest execution
  that stalls, with the L1 at 38 % user / 60 % system and 0 % idle. Disabling KVM halt-polling
  (`halt_poll_ns=0`) made it worse (N=10: 10.4 s), so it is not spin-waiting. The brief caps the
  L1 at 2 CPU / 2 GiB; a bigger L1 would move the knee, not remove the per-exit cost.
- **Cold boot 1.5 s** is the same effect across a whole kernel init (3.2 s with serial output
  on, because every console byte is an MMIO exit). Firecracker quotes "as little as 125 ms" to user space on bare metal ([firecracker-microvm.github.io](https://firecracker-microvm.github.io/)).

## What worked, what did not

**Worked, and removes ROADMAP work:**

1. **Nested KVM on M4 + macOS 26 + colima `--nested-virtualization`**: `/dev/kvm` exists and
   Firecracker runs unmodified. Option B is real.
2. **A stdlib `http.Client` with a unix `DialContext`** drove every call: boot-source, drives,
   machine-config, vsock, actions, `PATCH /vm` pause/resume, `snapshot/create` Full and Diff,
   `snapshot/load`. The whole driver was ~150 lines. `go.mod` stays at zero dependencies.
3. **The per-clone path problem is already solved upstream.** `SnapshotLoadParams` in the pinned
   `firecracker_spec-v1.17.0.yaml` has `vsock_override.uds_path` and `network_overrides`
   (host tap name), so N clones of one snapshot do not need a jailer or a mount namespace each
   just to avoid colliding on the vsock socket path. A **read-only** root drive makes the shared
   rootfs safe; a writable one needs a per-clone `FICLONE` copy at the same path (drive paths
   are in the snapshot state).
4. **The vermagic landmine does not fire with Firecracker's CI kernel**: its config has
   `CONFIG_VSOCKETS=y` and `CONFIG_VIRTIO_VSOCKETS=y` (built in, not modules), plus
   `VIRTIO_BLK`, `VIRTIO_NET`, `EXT4_FS`, `HW_RANDOM_VIRTIO`, `DEVTMPFS_MOUNT` all `=y`. Pinning
   *that* kernel (or its `.config`) instead of a distro kernel removes the failure mode. The
   aarch64 image starts `MZ` but bytes 4–7 are not `zimg`: it is a plain arm64 `Image`, no zboot
   unwrap needed for this one.
5. **Entropy on fork is half solved.** Firecracker v1.17 ships VMGenID and VMClock devices (both
   named in the binary; the guest logs `vmclock0: registered miscdev`) and the kernel has
   `CONFIG_VMGENID=y`. Measured across every clone at N=2..50: a fresh `getrandom` read was
   **distinct in every clone** (10/10, 20/20), and `/proc/sys/kernel/random/boot_id` was
   distinct (it is generated lazily on first read). **But a token the guest generated before the
   snapshot was identical in every clone** (`distinct boot_token = 1` at every N). The kernel
   reseeds; userspace does not. That is exactly execd's access token, Go's runtime hash seed and
   any cached key.

**Did not work / not attempted:**

- **Burst-TTI-class numbers on macOS.** 88 ms for one restore and seconds at N≥5; the 44 ms
  target is out of reach under nested virtualisation at this size.
- **Bare-metal Linux**: not measured - no Linux host with `/dev/kvm` was available to this
  spike. The same driver runs there unchanged.
- **Tap networking**: not exercised (vsock only). `network_overrides` exists for restore.
- **OCI → rootfs**: not exercised beyond `mkfs.ext4 -d` of a one-file tree (needs `e2fsprogs`
  on the host; the ROADMAP's userspace pipeline is still the bulk).
- **The Ubuntu CI rootfs** was fetched but not timed: an Ubuntu init would measure systemd, not
  the VMM, and the question was the wake path.

## execd as the guest agent

ROADMAP §1 budgets "a static musl binary on `AF_VSOCK` speaking a framed protocol" (3 wk).
execd already is the in-sandbox agent (compat design, `internal/execd`): an HTTP server that
backs command, filesystem and streaming endpoints. It can **be** the guest agent with only its
listener changed:

- **Guest side**: execd is PID 1 (or run by a 20-line init) and listens on `AF_VSOCK` instead
  of TCP. The spike's `/init` did exactly this with raw syscalls and no `x/sys`:

  ```go
  const afVsock = 40
  type sockaddrVM struct { Family, Reserved uint16; Port, CID uint32; Flags uint8; Zero [3]uint8 }

  fd, _ := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
  sa := sockaddrVM{Family: afVsock, Port: 44772, CID: 0xFFFFFFFF} // VMADDR_CID_ANY
  syscall.RawSyscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa))
  syscall.Listen(fd, 1024)
  // accept4(fd, nil, nil, SOCK_CLOEXEC) - syscall.Accept cannot parse an AF_VSOCK peer
  // wrap each conn with os.NewFile; serve HTTP on it (http.Serve over a small net.Listener shim)
  ```

- **Host side is a unix socket, not vsock.** Firecracker's hybrid vsock exposes the guest port
  as `<uds_path>` + a `CONNECT <port>\n` / `OK <n>\n` handshake. The wake proxy already splices
  bytes; it gains one handshake line and then splices to execd unchanged. No `AF_VSOCK` on the
  host, so no host kernel module either.
- **What the agent must add: re-key on fork.** Because userspace state is shared by every clone,
  execd needs a "you were restored as a new identity" step: either the host sends a new id/token
  over vsock immediately after `snapshot/load` (this is the same identity rewrite the warm-pool
  design already does through execd), or execd watches the VMGenID/VMClock generation. The first
  is simpler and already designed; do that.
- **Framed protocol vs HTTP**: not needed. execd's HTTP/SSE API over the vsock stream is the
  protocol, and the conformance suite already covers it.

## Revised estimate (ROADMAP §1 "The work")

| item | ROADMAP | revised | why |
|---|---|---|---|
| VMM driver | 2 wk | **1–1.5 wk** | every call needed was made from ~150 lines of stdlib; overrides remove the jailer-per-clone question |
| OCI → rootfs pipeline | 4 wk | 4 wk | not exercised; still the bulk |
| Guest kernel | 2 wk | **0.5–1 wk** | pin Firecracker's CI kernel + `.config`; vsock built in; this aarch64 image needs no zboot unwrap (keep the check for others) |
| Guest agent | 3 wk | **1.5 wk** | execd is the agent; the change is a vsock listener + the identity rewrite it already needs for the warm pool |
| Networking | 2 wk | 2 wk | not exercised |
| Wake path | 2 wk | **2.5 wk** | adds the fork re-key step, sparse/Diff snapshot handling, per-clone `FICLONE` for writable roots |
| doctor, refusals, tests | 3 wk | 3 wk | add: `/dev/kvm` + `KVM_GET_API_VERSION`=12 probe, nested-virt detection on darwin |
| **Linux total** | **≈18 wk** | **≈14.5–15.5 wk** | |
| macOS option B | +2–3 wk | **+2 wk** | colima `--nested-virtualization` gives a working `/dev/kvm` today; the work is provisioning the L1 and forwarding the UDS |

## Recommendation

1. **Build it, Linux-first (option A), but gate the build on one bare-metal run of this harness.**
   The design risk the ROADMAP names (VMM driver, vsock landmine, per-clone paths) came in
   smaller than budgeted. The remaining unknown is the headline number itself: whether
   restore → first byte on bare metal is inside 44 ms at N=100. This spike could not answer that
   and should not be read as answering it. A day on any Linux host with `/dev/kvm` answers it.
2. **Option B is a dev-parity story, not a speed story.** It works, and it is how a macOS user
   gets memory-inclusive snapshots at all (Virtualization.framework cannot, per option C). But
   88 ms single-restore and a multi-second burst mean the macOS Burst-TTI path stays the
   **docker warm pool** from the compat design; a microVM provider on macOS must not be sold as
   faster than a frozen container.
3. **Keep C unbuilt.** Nothing here changes that.

## Reproducing

Throwaway programs (not in the repo): a KVM ioctl probe, the guest `/init` above, and the driver
(`fcbench`: `mkSnapshot` → interleaved cold/restore rounds → concurrent N → identity check via
`GET /id` returning a pre-snapshot token, a fresh `getrandom`, and `boot_id`). Run in the L1 as
root with the snapshot on disk (`/var`, not the 392 MiB `/run` tmpfs, which would hold `vm.mem`
in RAM). The `fc` colima profile was stopped afterwards; `default` and `osb` were not touched.
