# Security

## Reporting

Report a vulnerability through [GitHub's private advisory
form](https://github.com/aryanmehrotra/sbx/security/advisories/new). Please don't open a
public issue for something exploitable.

Expect an acknowledgement within about a week. If a report is confirmed, the fix and the
advisory go out together.

## Advisories

### v0.9.0: a keyless OpenSandbox API is reachable from every container (fixed in v0.9.1)

**Affected:** v0.9.0, only when `sbx serve --osb-addr` was started **without `--osb-key`** (and
without `SBX_OSB_KEY`). v0.9.0 allowed that on a loopback address. Nothing else in sbx is
affected; without `--osb-addr` the API does not exist.

**What was exposed.** v0.9.0 treated `127.0.0.1` as private. On a VM-backed engine - colima,
Docker Desktop - it is not: every container reaches the host's loopback through
`host.docker.internal`, `host.lima.internal` or the VM gateway (`192.168.5.2` on colima), and the
connection arrives from `127.0.0.1`. So any container on that engine, including an API sandbox
running untrusted code, could call the keyless lifecycle API: list sandboxes, read every
sandbox's execd token from `GET /v1/sandboxes/{id}/endpoints/44772`, and with it run commands in
and read or write files of **any other sandbox**; create and delete sandboxes; and, through the
egress sidecar route (authenticated by the execd token its own environment holds), lift its own
egress policy.

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

## What sbx assumes

Most of what people expect to be a vulnerability here is a documented design position, so
this section exists to make the boundary explicit before you look for the boundary yourself.

**sbx is a tool you run on hardware you control. It is not a multi-tenant platform, and the
threat model is not "untrusted users share one daemon".**

- **No authentication, no per-user isolation, no quotas.** Anyone who can reach the daemon's
  ports can use any sandbox on that machine, and anyone who can run `sbx` can destroy any of
  them. This is deliberate and is not going to change -
  [DECISIONS.md](docs/DECISIONS.md#sbx-is-a-tool-people-run-not-a-service-anyone-offers) has
  the reasoning. The supported shape is a machine whose users already trust each other.
- **Ports bind to loopback only.** `sbx serve` listens on `127.0.0.1`, and docker publishes
  backing ports on `127.0.0.1`. Nothing is exposed to the network unless you deliberately
  expose it. Two things do, and both are opt-in and say so:
  - `sbx url` - one HTTP service, per invocation, printing the URL it created.
  - `sbx serve --connect-addr` - the tunnel endpoint for a *deployed* sbx, off unless the flag
    is passed. It refuses to start without `SBX_CONNECT_TOKEN`, and refuses a non-loopback
    address unless `--behind-proxy` says something in front terminates TLS. The sandbox ports
    themselves stay on loopback: the endpoint is the only thing listening outward, and what it
    carries is a TCP stream to a port it is already fronting.
- **The connect token is the whole boundary.** Anyone holding it has TCP access to every
  service in that deployment - the services' own credentials still apply, but sbx does not
  check them. That is the same posture as an SSH key on a dev box, and it is why the control
  plane (`create`, `rm`, `exec`) is deliberately *not* reachable over it.
- **`--front host:port` makes the deployment a bridgehead, and that is a bigger claim than
  the rest of this list.** A bare `--front 5432` can only reach a process on the daemon's own
  loopback - something in its container, which you put there. Naming a host lets it reach
  anything it can *route* to: the platform's database on a private address, a peer service, a
  neighbouring subnet. That is the point of it, and it is also the cost. The token no longer
  gates "the services in this deployment"; it gates "everything this container's network
  position can reach on the ports you listed", and the two are only the same thing when the
  list is loopback.

  So: front the ports you actually need, not a convenient range, and treat
  `SBX_CONNECT_TOKEN` on a host-fronting deployment as you would a VPN credential rather than
  a service password. sbx will not stop you fronting a whole private network - it cannot know
  which addresses you meant - which is exactly why the decision is written in the deployment's
  own environment, where a reviewer can see it, rather than inferred at run time.
- **A container shares the host kernel.** `--isolation gvisor|kata` asks for a stronger
  boundary and is *refused with a reason* where the runtime is absent rather than silently
  downgraded. If you are running code you did not write, use one of those or use a tool built
  on microVMs; [COMPARISON.md](docs/COMPARISON.md) names them.
- **A microVM sandbox (`--provider firecracker`) is on the host's network, not behind it.**
  Each sandbox is a bridge (`10.231.<slot>.0/24`, the host at `.1`) with no NAT, so a guest has no
  route off the host; a filtered one (`egress_allow`, `egress_policy`, `egress: "allow"`) reaches
  the internet only through the egress filter on `10.231.<slot>.1:20999`, which refuses the host's
  own addresses, its loopback, every other sandbox's guests, private ranges (RFC 1918, CGNAT, ULA)
  and every neighbour on a host interface's subnet - and no rule in the sandbox's own policy can
  open them. Only the operator can, for private ranges only: `sbx serve --vm-egress-allow <CIDR,...>`. Two consequences:
  - **The host is closed to a guest except for that filter port** - where sbx could install it.
    Each bridge gets a chain `SBX-FC<slot>` in INPUT (replies returned to your rules, the filter
    port accepted, the rest dropped) and another in mangle PREROUTING that drops what a guest starts
    before docker's DNAT can turn it into forwarded traffic - so a docker-published port, a
    container's IP and a NodePort are closed too, not only services bound to the host - plus
    mangle FORWARD drops from and to the bridge, and IPv6 off - made with the bridge, re-checked on
    every wake and daemon reconcile (a flushed rule is put back), and removed with it
    (DECISIONS.md, "A microVM's only door is its filter"). **It fails closed (v0.13):** where
    `iptables` is missing, a rule is refused, IPv6 cannot be turned off on the bridge, or a flushed
    guard cannot be put back, the create or wake is **refused** with the reason and the fix, and no
    bridge is left up unguarded. (v0.12 warned and booted, leaving every host service bound to
    `0.0.0.0` reachable at `10.231.<slot>.1`.) `sbx doctor` shows it (`vm host guard`). A bridge
    already in use whose guard the daemon's reconcile cannot put back keeps its running VMs, logged,
    and no new VM starts on it; an API sandbox's Running status message and history carry
    that warning, so its caller sees it too. Bridges made by v0.11 are guarded on their next wake.
  - **`--fc-firewall=unmanaged` (`SBX_FC_FIREWALL=unmanaged`) hands all of that to you.** For
    a host whose own firewall is the authority: sbx then writes no rule at all, and closing the host
    to `10.231.0.0/16` - INPUT, and anything (docker, kube-proxy) that DNATs a guest's packet past
    INPUT - is the operator's responsibility, not sbx's.
  - **Isolation between sandboxes is sbx's where the guard is installed** (the mangle FORWARD
    drops), and the host's FORWARD policy where it is not. With `ip_forward=1` (docker turns it on)
    an unmanaged host's bridges can reach each other unless the policy is `DROP`. `sbx doctor` checks it (`vm bridges isolated`), and every
    create warns when it is not confirmed.
- **`sbx serve --provider firecracker` runs as root** (or with `CAP_NET_ADMIN`): it makes a tap
  and a bridge per sandbox and writes the iptables rules that guard them, and `--osb-addr` is
  refused at startup without that privilege rather than failing every create on its tap.
- **Every VMM runs under Firecracker's jailer (v0.13, on by default).** v0.12's accepted risk - the
  VMM as unconfined root, so a guest-to-VMM escape landed as root on the host - is closed where the
  jailer is on. Each VMM is started through the jailer pinned from the same v1.17.0 release
  tarball (same sha256): chrooted into `<vm dir>/jail/firecracker/<id>/root`, which holds only its
  kernel (a link to the one root-owned, read-only file every VM shares - never re-owned, a symlink
  resolved first, a root-owned copy when it is not that, and held to root's and read-only again
  after the jailer has run), its own drives and snapshot files (hard links, owned by its uid -
  except what it only reads: the agent drive and read-only volumes stay root's, readable, never
  writable, by the jail, so a compromised VMM cannot rewrite a read-only volume for the next
  sandbox), `/dev/kvm`, `/dev/net/tun`, `/dev/urandom` and its sockets; running as
  **its own uid and gid** (`900000 + slot*256 + index`, never 0, never shared by two VMs, so one VMM
  cannot signal, trace or open another's files), with no capabilities; in its own cgroup v2
  (`cpu.max` from the spec's CPUs, `memory.max` its memory + 128 MiB). What it writes (a snapshot)
  is taken back only as a plain file with one name, so a compromised VMM cannot plant a symlink or
  hard link for the host to follow as root. Each VM's record says how its running VMM was launched
  (`jail_uid`), and every later call to it speaks in that mode, whatever `SBX_FC_JAILER` says now. A
  VM resumed in place (a paused one started, a frozen warm-pool member claimed) gets the same
  host-guard recheck as a wake. `sbx doctor` shows it (`vm jailer`).
  **What remains:**
  - **`SBX_FC_JAILER=off` restores v0.12's risk exactly**, for a development host where the jailer
    cannot run (no cgroup v2, no mknod). Warned on every use; the OpenSandbox API refuses to serve on
    firecracker with it unless `--osb-insecure-no-jailer` is passed.
  - **No network namespace per VM.** The VMM shares the host's; its tap on the sandbox's bridge is
    guarded as above. A VMM escape reaches what a non-root uid on the host's network can.
  - **The uid range must hold no real account** (`SBX_FC_JAILER_UID_BASE` moves it); sbx does not
    check `/etc/passwd`.
  - **A compromised VMM owns its chroot's `/`** and can replace its API or vsock socket there with
    a symlink to another VM's (the path is predictable). sbx no longer follows it: it takes its own
    `<vm dir>/api.sock` / `vsock.sock` link into the jail, and then requires the entry in the root to
    be a socket (not a symlink) owned by the jail's uid, and - on Linux - the process that answers
    to be that uid (`SO_PEERCRED`, which a swap between the check and the connect cannot fake).
    Anything else is refused as a foreign socket, never read as "asleep". What such a VMM can still
    do is refuse to answer, or answer its own API wrongly - about itself only.
  - **No disk quota per VM.** A VM's disk, memory snapshot and anything its VMM writes in its jail
    (as its own uid) are on the state filesystem (`SBX_FC_STATE`), with no per-VM or per-uid limit:
    one sandbox - or a compromised VMM - can fill that filesystem (ENOSPC) for the host and every
    other sandbox on it. Where that is `/`, the host itself. The mitigation is the operator's: put
    `SBX_FC_STATE` on a filesystem of its own (a dedicated partition or volume), or enable project
    quotas (XFS / ext4 `prjquota`) on it. `sbx doctor` warns when the state directory shares `/`
    (`microVM state filesystem`).
  - **The daemon still runs as root** (taps, bridges, iptables, the jailer itself), and the guest
    kernel plus Firecracker's own seccomp filters are the first boundary, as before.
  - Snapshots taken before v0.13 name host paths the jailed VMM cannot open: those VMs cold-boot
    once (memory lost, disk kept), and a saved memory snapshot of the other kind is refused by name.
- **In a microVM, the guest's root can read execd's own secrets** - the access token and the
  boot control secret, from `/proc/1/environ` and `/init.json` on the agent drive. execd strips
  them from what it starts, which keeps them out of `env` and logs, not from root. They are
  harmless there by construction: each belongs to that guest alone, control is reachable only
  over vsock from the host, the control secret is rotated at every restore and every snapshot
  (the running VM and a saved one never share it), and forking a VM's memory is refused (a
  snapshot fork copies the disk only). Neither can be chosen by anyone else: sbx strips both from
  the image's ENV and the spec's env before appending its own (getenv takes the first occurrence,
  so an image's would otherwise have won), and the OpenSandbox API refuses a create that sets
  either (400).
- **`egress: "deny"` is coarse.** It removes routed egress by putting the service on a bridge
  with IP masquerade disabled. It is not a filtering firewall: it cannot allow one domain and
  deny another, and it is enforced by docker's networking rather than by anything sbx
  supervises. On kubernetes it is **refused** rather than approximated, because a NetworkPolicy
  is only enforced by some CNIs and a security control that silently did nothing is worse than
  one that says no.
- **Specs are executable.** `sandbox.json` names images to run, commands to run inside them
  (`health`, `init`) and host files to mount. Treat a spec from someone else exactly as you
  would treat their Makefile or their `docker-compose.yml`.
- **`${VAR}` reads your environment.** It exists so a committed spec can name a secret without
  holding one. The value still reaches the container's environment, which is visible to
  anything that can `docker inspect` it.

## What would be a real vulnerability

Roughly: anything that breaks a boundary sbx claims to hold.

- A sandbox reaching another sandbox's data, or a fork inheriting state it should not.
- An OpenSandbox `host` volume binding anything but the directory the API validated under
  `--osb-host-paths` - at create, or at any later start of the same container. (Before v0.13 a
  sandbox woken from `sbx.idle=sleep` followed a symlink swapped in while it slept, and on docker
  the re-check just before create did not run; both are fixed in v0.13 -
  [DECISIONS.md](docs/DECISIONS.md#volumes-on-the-api-host-paths-are-the-operators-to-allow-and-claims-are-namespaced).)
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
- Anything in the spec reaching a shell it should not - the values are passed as arguments,
  not interpolated into a command line, and a case where that is not true is a bug.
- Path traversal out of `~/.sbx`, or a sandbox/service/snapshot name that escapes the
  container, volume or image name it is meant to become.

Several of these are pinned by tests that were written by breaking the code and confirming
the test failed. That does not mean they are all correct - it means the intent is written
down and checked.

## Supported versions

Pre-1.0: fixes land on `main` and there is no backport branch. Use the latest tag. A security fix
may also ship as a patch on the latest release (v0.9.1 is one).
