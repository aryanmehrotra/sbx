# Security

How to report a vulnerability in sbx, which versions get fixes, and the boundaries sbx does and
does not claim to hold. For operators deciding how far to trust it, and for security reviewers.

## Reporting

Report a vulnerability through [GitHub's private advisory
form](https://github.com/aryanmehrotra/sbx/security/advisories/new). Please don't open a
public issue for something exploitable.

Expect an acknowledgement within about a week. If a report is confirmed, the fix and the
advisory go out together.

## Supported versions

Pre-1.0: fixes land on `main` and there is no backport branch. The supported version is the
latest release, currently **v0.14.x**; use the latest tag. A security fix may also ship as a patch
on the latest release (v0.9.1 is one).

| version | supported |
|---|---|
| latest release (v0.14.x) | yes |
| anything older | no: upgrade |

Published advisories: [v0.9.0, a keyless OpenSandbox API reachable from every
container](#v090-a-keyless-opensandbox-api-is-reachable-from-every-container-fixed-in-v091)
(fixed in v0.9.1). Details are [at the end of this page](#advisories).

## What sbx assumes

Most of what people expect to be a vulnerability here is a documented design position. This
section states the boundary up front, so you do not have to find it yourself.

**sbx is a tool you run on hardware you control. It is not a multi-tenant platform, and the
threat model is not "untrusted users share one daemon".**

### The boundary at a glance

| Runs on | What separates a sandbox from the host | Holds | Does not hold |
|---|---|---|---|
| docker or kubernetes (default) | A container: its own filesystem and processes, the host's kernel | Separation between cooperating projects on your machine | Code you did not write: a kernel bug escapes to the host |
| `--isolation gvisor` or `kata` | gVisor (a user-space kernel) or Kata (a small VM per container) | More than a plain container; refused with a reason where the runtime is missing | Only what that runtime claims |
| `--provider firecracker` | A microVM (a small virtual machine with its own kernel), its VMM locked down per VM | Untrusted code: an escape lands as an unprivileged, jailed user | The daemon runs as root; no per-VM disk quota on the state filesystem (details below) |

A VMM (virtual machine monitor) is the host process that runs one VM; here it is Firecracker.

### Access and exposure

- **No authentication, no per-user isolation, no quotas.** Anyone who can reach the daemon's
  ports can use any sandbox on that machine. Anyone who can run `sbx` can destroy any of them.
  This is deliberate and is not going to change;
  [DECISIONS.md](docs/DECISIONS.md#sbx-is-a-tool-people-run-not-a-service-anyone-offers) has
  the reasoning. The supported shape is a machine whose users already trust each other.
- **Ports bind to loopback only.** `sbx serve` listens on `127.0.0.1`, and docker publishes
  backing ports on `127.0.0.1`. Nothing reaches the network unless you expose it. Two opt-in
  features do, and both say so:
  - `sbx url` exposes one HTTP service per invocation and prints the URL it created.
  - `sbx serve --connect-addr` is the tunnel endpoint for a *deployed* sbx, off unless the flag
    is passed. It refuses to start without `SBX_CONNECT_TOKEN`. It refuses a non-loopback
    address unless `--behind-proxy` says something in front terminates TLS. The sandbox ports
    stay on loopback: the endpoint is the only thing listening outward, and it carries a TCP
    stream to a port it already fronts.
- **The connect token is the whole boundary.** Anyone holding it has TCP access to every
  service in that deployment. The services' own credentials still apply, but sbx does not check
  them; that is the same posture as an SSH key on a dev box. The token also **controls** the
  deployment: wake, sleep, re-limit, remove and logs (`/v1/control/*`,
  `internal/daemon/control.go`) pass the same check. So a leaked token can remove sandboxes, not
  only reach them. `create` and `exec` are not reachable over it.
- **`--front host:port` turns the deployment into a bridgehead.** That is a bigger claim than the
  rest of this list.
  - A bare `--front 5432` reaches only a process on the daemon's own loopback: something in its
    container, which you put there.
  - Naming a host lets it reach anything it can *route* to: the platform's database on a private
    address, a peer service, a neighbouring subnet. That is the point of it, and also the cost.
  - The token then gates "everything this container's network position can reach on the ports
    you listed", not just "the services in this deployment".

  So front only the ports you need, not a convenient range. Treat `SBX_CONNECT_TOKEN` on a
  host-fronting deployment like a VPN credential, not a service password. sbx cannot know which
  addresses you meant, so it will not stop you fronting a whole private network. That is why the
  choice is written in the deployment's own environment, where a reviewer can see it.
- **`sbx mcp` holds the OpenSandbox API key.** It is a client of the API, so an agent driving
  it can do whatever the key allows: create, exec in and delete API sandboxes. Point it at a
  remote API with `--url` only over a transport you trust.

### Containers

- **A container shares the host kernel.** For code you did not write, use
  **`--provider firecracker`** (next section). It runs on Linux with `/dev/kvm`, or on an M3+ Mac
  or Windows 11 through a helper VM (a Linux VM sbx runs for you). `--isolation gvisor|kata` is
  the alternative on docker or kubernetes. Where the runtime is absent it is *refused with a
  reason*, never silently downgraded.
- **`egress: "deny"` is coarse.** It removes routed egress (traffic leaving the sandbox) by putting
  the service on a bridge with IP masquerade disabled. It is not a filtering firewall: it cannot
  allow one domain and deny another. Docker's networking enforces it, not anything sbx
  supervises. On kubernetes it is **refused** rather than approximated: a NetworkPolicy is only
  enforced by some CNIs, and a security control that silently does nothing is worse than a no.
- **Specs are executable.** `sandbox.json` names images to run, commands to run inside them
  (`health`, `init`) and host files to mount. Treat a spec from someone else exactly as you
  would treat their Makefile or their `docker-compose.yml`.
- **`${VAR}` reads your environment.** It lets a committed spec name a secret without holding
  one. The value still reaches the container's environment, which anything that can
  `docker inspect` it can read.

### MicroVMs (`--provider firecracker`)

Each service is a microVM with its own guest kernel. Its VMM runs jailed as its own user with no
capabilities, in its own network namespace (a private copy of the network stack), behind a host
guard that fails closed. What is still open, all detailed below: the daemon runs as root; there
is no per-VM quota on the state filesystem; `SBX_FC_JAILER=off` removes the jail; and isolation
between bridges depends on the guard being installed.

**A microVM sandbox is on the host's network, not behind it.**

- Each sandbox is a bridge (`10.231.<slot>.0/24`, the host at `.1`) with no NAT, so a guest has
  no route off the host.
- A filtered sandbox (`egress_allow`, `egress_policy`, `egress: "allow"`) reaches the internet
  only through the egress filter on `10.231.<slot>.1:20999`.
- That filter refuses the host's own addresses, its loopback, every other sandbox's guests,
  private ranges (RFC 1918, CGNAT, ULA) and every neighbour on a host interface's subnet. No rule
  in the sandbox's own policy can open them.
- Only the operator can, and for private ranges only: `sbx serve --vm-egress-allow <CIDR,...>`.

**The host is closed to a guest except for that filter port**, where sbx can install the guard.
For each bridge it adds:

- a chain `SBX-FC<slot>` in INPUT: replies go back to your own rules, the filter port is
  accepted, the rest is dropped;
- a chain in mangle PREROUTING that drops what a guest starts before docker's port forwarding
  (DNAT) can turn it into forwarded traffic. So a docker-published port, a container's IP and a
  NodePort are closed too, not only services bound to the host;
- mangle FORWARD drops from and to the bridge, and IPv6 switched off on it.

The guard is made with the bridge and removed with it. It is re-checked on every wake and every
daemon reconcile, and a flushed rule is put back (DECISIONS.md, "A microVM's only door is its
filter"). `sbx doctor` shows it (`vm host guard`).

**The guard fails closed.** If `iptables` is missing, a rule is refused, IPv6 cannot be switched
off on the bridge, or a flushed guard cannot be put back, the create or wake is **refused** with
the reason and the fix. No bridge is left up unguarded. (Before v0.13 it warned and booted,
leaving every host service bound to `0.0.0.0` reachable at `10.231.<slot>.1`.) A bridge already in
use whose guard cannot be put back keeps its running VMs, logged, and no new VM starts on it. An
API sandbox's Running status message and history carry that warning, so its caller sees it too.
Bridges made by v0.11 are guarded on their next wake.

**`--fc-firewall=unmanaged` (`SBX_FC_FIREWALL=unmanaged`) hands all of that to you.** It is for a
host whose own firewall is the authority. sbx then writes no rule at all. Closing the host to
`10.231.0.0/16` is the operator's job: INPUT, and anything (docker, kube-proxy) that rewrites a
guest's packet past INPUT.

**Isolation between sandboxes** comes from the guard's mangle FORWARD drops where it is
installed, and from the host's FORWARD policy where it is not. With `ip_forward=1` (docker turns
it on), an unmanaged host's bridges can reach each other unless that policy is `DROP`.
`sbx doctor` checks it (`vm bridges isolated`), and every create warns when it is not confirmed.

**`sbx serve --provider firecracker` runs as root** (or with `CAP_NET_ADMIN`). It makes a tap
(a virtual network card) and a bridge per sandbox and writes the iptables rules that guard them.
`--osb-addr` is refused at startup without that privilege, rather than failing every create.

### How the microVM boundary is built

**Every VMM runs under Firecracker's jailer, on by default since v0.13.** The jailer is
Firecracker's launcher: it locks each VM's process into its own directory as an unprivileged
user. Before v0.13 the VMM ran as unconfined root, so a guest-to-VMM escape landed as root on the
host; where the jailer is on, that is closed. The jailer comes from the same pinned v1.17.0
release tarball as the VMM (same sha256). Each jailed VMM gets:

- **A chroot** (a directory it sees as `/`): `<vm dir>/jail/firecracker/<id>/root`. It holds only:
  - its kernel: a link to the one root-owned, read-only file every VM shares. A symlink is
    resolved first; anything else gets a root-owned copy. It is never given to the VM's user, and
    is made root's and read-only again after the jailer runs;
  - its image's root filesystem: for a layered VM, the base shared by every VM of that image,
    held exactly like the kernel (root's, read-only);
  - its own drives and snapshot files, as hard links owned by its user. The agent drive and
    read-only volumes stay root's: readable by the jail, never writable. So a compromised VMM
    cannot rewrite a read-only volume for the next sandbox;
  - `/dev/kvm`, `/dev/net/tun`, `/dev/urandom` and its sockets.
- **Its own user and group ID**: `900000 + slot*256 + index`. Never 0, never shared by two VMs, so
  one VMM cannot signal, trace or open another's files.
- **No capabilities** (Linux's split-up root privileges).
- **Its own cgroup v2** (a kernel resource limit): `cpu.max` from the spec's CPUs, `memory.max`
  its memory plus 128 MiB.
- **A network namespace per VM** (v0.14): it holds only the VM's tap (owned by its user),
  bridged to a veth (a virtual cable) into the sandbox's guarded bridge, with no address and no
  route. A VMM escape reaches what the guest reaches, not the host's network. Unjailed, the VMM
  shares the host's namespace and reaches what a root process can.

Also:

- What a VMM writes (a snapshot) is taken back only as a plain file with one name, so a
  compromised VMM cannot plant a symlink or hard link for the host to follow as root.
- Each VM's record says how its running VMM was launched (`jail_uid`). Every later call to it uses
  that mode, whatever `SBX_FC_JAILER` says now.
- A VM resumed in place (a paused one started, a frozen warm-pool member claimed) gets the same
  host-guard recheck as a wake. `sbx doctor` shows the jailer (`vm jailer`).

**What remains:**

- **`SBX_FC_JAILER=off` restores the pre-v0.13 risk exactly.** It is for a development host where
  the jailer cannot run (no cgroup v2, no mknod). It warns on every use, and the OpenSandbox API
  refuses to serve on firecracker with it unless `--osb-insecure-no-jailer` is passed.
- **The user ID range must hold no real account** (`SBX_FC_JAILER_UID_BASE` moves it). sbx does
  not check `/etc/passwd`.
- **A compromised VMM owns its chroot's `/`.** It can replace its API or vsock socket there with a
  symlink to another VM's, since the path is predictable. sbx does not follow it:
  - it reaches the socket through its own `<vm dir>/api.sock` / `vsock.sock` link into the jail;
  - it requires the entry to be a socket (not a symlink) owned by the jail's user;
  - on Linux, it requires the answering process to be that user (`SO_PEERCRED`, which a swap
    between the check and the connect cannot fake).

  Anything else is refused as a foreign socket, never read as "asleep". Such a VMM can still
  refuse to answer, or answer its own API wrongly, about itself only.
- **A VM's disk is bounded; a compromised VMM's jail is bounded per file, not in total.**
  - A guest writes only fixed-size drives: its writable layer (`SBX_FC_DISK_SIZE`, 10G by
    default, sparse; the image beneath is shared and read-only), its volumes
    (`SBX_FC_VOLUME_SIZE`), and when it sleeps a memory file as big as its RAM. Its console is
    cut back past 16 MiB.
  - A jailed VMM runs with `RLIMIT_FSIZE` (the jailer's `--resource-limit fsize=`) set to the
    largest of those files plus 64 MiB. No one file it writes in its jail can grow past that; the
    VMM is killed (SIGXFSZ). Unjailed (`SBX_FC_JAILER=off`), there is no file-size limit.
  - Such a VMM can still make **many** files in the jail root it owns. Many VMs together can
    still fill the state filesystem (`SBX_FC_STATE`): there is no per-user or per-VM quota. Where
    that is `/`, it is the host itself.
  - The mitigation is the operator's: put `SBX_FC_STATE` on its own filesystem (a dedicated
    partition or volume), or enable project quotas (XFS or ext4 `prjquota`) on it. `sbx doctor`
    warns when the state directory shares `/` (`microVM state filesystem`).
- **The daemon still runs as root.** The guest kernel and Firecracker's own seccomp filters (a
  kernel allow-list of system calls) are the first boundary. The daemon cannot give root up after
  startup: every create and wake does privileged work again (a namespace, a veth and a tap, the
  guard's rules, a jail to fill and a jailer to run). So "set up, then drop" has no "after".
  Below is what it needs, and why: the set a host can bound it to with a system unit's
  `CapabilityBoundingSet=`. CI runs it as full root, so a bounded daemon is not exercised there.

  | capability | for |
  |---|---|
  | `CAP_NET_ADMIN` | bridges, veths, taps, `ip netns`, iptables rules, the bridge's IPv6 sysctl |
  | `CAP_NET_RAW` | iptables' legacy backend (raw sockets) |
  | `CAP_SYS_ADMIN` | `ip netns add` (a bind mount of the namespace), and the jailer: mount namespace, pivot_root, setns |
  | `CAP_SYS_CHROOT` | the jailer's chroot |
  | `CAP_MKNOD` | the jailer's `/dev/kvm`, `/dev/net/tun`, `/dev/urandom` in the jail |
  | `CAP_SETUID`, `CAP_SETGID` | the jailer dropping to the VM's user and group |
  | `CAP_CHOWN`, `CAP_FOWNER` | giving a VM its own files and taking them back; keeping the shared kernel and base root's and 0444 |
  | `CAP_DAC_OVERRIDE` | reading what a jailed VMM wrote, connecting to its sockets, the cgroup tree |
  | `CAP_KILL` | ending a VMM that runs as another user |
  | `CAP_SYS_RESOURCE` | the jailer's `no-file` limit where the hard limit is lower |

  `CAP_SYS_ADMIN` is most of root, so the bound narrows little. The real reduction is that no
  VMM holds any of it: `CapEff` is 0 in every jailed VMM, checked from `/proc` in CI.
- Snapshots taken before v0.13 name host paths the jailed VMM cannot open. Those VMs cold-boot
  once (memory lost, disk kept), and a saved memory snapshot of the other kind is refused by name.
- **In a microVM, the guest's root can read the in-VM agent's own secrets.** The agent (execd)
  runs commands for the API. Its access token and boot control secret sit in `/proc/1/environ`
  and `/init.json` on the agent drive. execd strips them from what it starts, which keeps them out
  of `env` and logs, but not from root. They are harmless there by construction:
  - each belongs to that guest alone, and control is reachable only over vsock from the host;
  - the control secret is rotated at every restore and every snapshot, so the running VM and a
    saved one never share it;
  - forking a VM's memory is refused (a snapshot fork copies the disk only);
  - nobody else can choose them. sbx strips both from the image's ENV and the spec's env before
    appending its own (getenv takes the first occurrence, so an image's would otherwise win). The
    OpenSandbox API refuses a create that sets either (400).

## What would be a real vulnerability

Roughly: anything that breaks a boundary sbx claims to hold.

- A sandbox reaching another sandbox's data, or a fork inheriting state it should not.
- An OpenSandbox `host` volume binding anything but the directory the API validated under
  `--osb-host-paths`, at create or at any later start of the same container. Two such bugs were
  fixed in v0.13: a sandbox woken from sleep followed a symlink swapped in while it slept, and on
  docker the re-check just before create did not run
  ([DECISIONS.md](docs/DECISIONS.md#volumes-on-the-api-host-paths-are-the-operators-to-allow-and-claims-are-namespaced)).
- A container reaching the OpenSandbox API (`--osb-addr`) without the key, or getting from inside
  its sandbox a credential that works anywhere but that sandbox's own execd.
- `egress: "deny"` permitting routed egress on docker.
- A filtered service (`egress_policy`, `egress_allow`, `egress: "allow"`) reaching a destination its
  policy denies - directly, through the filter, or through a name that resolves into a denied
  range - or rewriting its own policy through the filter's control endpoint.
- `--isolation gvisor|kata` reporting success while running under the default runtime.
- A public port serving a different sandbox than the one `sbx env` named - including a
  `sbx connect` tunnel still carrying traffic to a port whose sandbox was recreated under it.
- `sbx gc` deleting an artifact belonging to a live sandbox.
- Reaching the `sbx connect` endpoint's tunnel or its `/v1/control/*` routes without the token,
  or that endpoint listening off loopback without `--behind-proxy`.
- On `--provider firecracker`, with the jailer on:
  - a jailed VMM holding any capability (`CapEff` not 0), running as uid 0 or as another VM's uid,
    or writing outside its own jail;
  - a guest reaching the host, another sandbox's guest, or a private range past its filter and
    the host guard, other than through `--vm-egress-allow`;
  - a create or wake succeeding with the host guard missing, when the firewall is managed;
  - a restored or forked VM sharing execd's token or control secret with another VM.
- Anything in the spec reaching a shell it should not - the values are passed as arguments,
  not interpolated into a command line, and a case where that is not true is a bug.
- Path traversal out of `~/.sbx`, or a sandbox/service/snapshot name that escapes the
  container, volume or image name it is meant to become.

Several of these are pinned by tests that were written by breaking the code and confirming
the test failed. That does not mean they are all correct - it means the intent is written
down and checked.

## Advisories

### v0.9.0: a keyless OpenSandbox API is reachable from every container (fixed in v0.9.1)

**Affected:** v0.9.0, only when `sbx serve --osb-addr` was started **without `--osb-key`** (and
without `SBX_OSB_KEY`). v0.9.0 allowed that on a loopback address. Nothing else in sbx is
affected; without `--osb-addr` the API does not exist.

**What was exposed.** v0.9.0 treated `127.0.0.1` as private. On a VM-backed engine - colima,
Docker Desktop - it is not: every container reaches the host's loopback through
`host.docker.internal`, `host.lima.internal` or the VM gateway (`192.168.5.2` on colima), and the
connection arrives from `127.0.0.1`. So any container on that engine, including an API sandbox
running untrusted code, could call the keyless lifecycle API. From there it could:

- list sandboxes, and read every sandbox's execd token (the in-sandbox agent's credential) from
  `GET /v1/sandboxes/{id}/endpoints/44772`;
- with that token, run commands in and read or write files of **any other sandbox**;
- create and delete sandboxes;
- through the egress sidecar route (authenticated by the execd token its own environment holds),
  lift its own egress policy.

**Impact:** cross-sandbox takeover and egress-policy bypass from inside a sandbox. **No host
escape** in v0.9.0: its API accepts no volumes (they answer 501), no capabilities and no
privileged mode, so a sandbox created through it is an ordinary container. (v0.10.0 adds host
volumes, only under roots named by `--osb-host-paths`; a keyless API there would have reached
those directories - one reason the key requirement ships in v0.10.0 as well.)

**Fixed in v0.9.1:**

- a key is always required - generated into `~/.sbx/osb/key` (0600) when none is given;
  keyless only with an explicit `--osb-insecure-no-key`, loopback only, loudly;
- the egress sidecar route has its own per-sandbox credential that is never in the container;
- a non-loopback `--osb-addr` is refused until server-proxy mode exists;
- API pauses are applied before the daemon binds any listener, and `sbx sleep`/`wake` and the
  control API refuse to undo them;
- API sandboxes carry an `sbx.osb` label and an unscoped daemon that does not serve the API
  leaves them alone. Sandboxes created by v0.9.0 have no label: recreate them to get this.

**Workaround on v0.9.0:** always pass `--osb-key` (or set `SBX_OSB_KEY`) to a random value, and
give that key to your clients (`OPEN_SANDBOX_API_KEY`, `sbx mcp --key`). Sandboxes that ran
untrusted code on a keyless v0.9.0 API should be treated as able to have touched every other API
sandbox on that engine.

Why loopback is not a trust boundary here, and what follows from it:
[DECISIONS.md](docs/DECISIONS.md#loopback-is-not-a-trust-boundary-on-a-vm-backed-engine).
