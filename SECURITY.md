# Security

How to report a vulnerability, which versions get fixes, and which boundaries sbx holds.

## Reporting

Report a vulnerability through [GitHub's private advisory
form](https://github.com/aryanmehrotra/sbx/security/advisories/new). Please don't open a public
issue for something exploitable. Expect an acknowledgement within about a week. A confirmed fix
and its advisory go out together.

## Supported versions

| Version | Supported |
|---|---|
| latest release (v0.15.x) | yes |
| anything older | no: upgrade |

Before 1.0, fixes land on `main` with no backport branch. A security fix may also ship as a patch
on the latest release, as v0.9.1 did. Published advisories are [at the end of this page](#advisories).

## The boundary per backend

sbx is a tool you run on hardware you control. It is not a multi-tenant platform.

| Runs on | What separates a sandbox from the host | Holds | Does not hold |
|---|---|---|---|
| docker or kubernetes (default) | a container: own filesystem and processes, the host's kernel | separation between cooperating projects | code you did not write: a kernel bug escapes to the host |
| `--isolation gvisor` or `kata` | gVisor (a user-space kernel) or Kata (a small VM per container) | more than a container; refused with a reason where the runtime is missing | anything beyond what that runtime claims |
| `--provider firecracker` | a microVM with its own kernel, its VMM jailed per VM | untrusted code: an escape lands as an unprivileged, jailed user | the daemon runs as root; no per-VM disk quota on the state filesystem |

A VMM (virtual machine monitor) is the host process that runs one VM; here it is Firecracker.

### Containers

- A container shares the host kernel. For code you did not write, use `--provider firecracker`
  (Linux with `/dev/kvm`, or an M3+ Mac or Windows 11 through a helper VM). `--isolation
  gvisor|kata` is the alternative on docker or kubernetes. A missing runtime is refused, never
  silently downgraded.
- `egress: "deny"` puts the service on a bridge with IP masquerade disabled. It is not a
  filtering firewall, and docker enforces it, not sbx. On kubernetes it is refused, because a
  NetworkPolicy is enforced only by some CNIs.
- A filtered service (`egress_allow`, `egress_policy`, `egress: "allow"`) reaches out only through
  sbx's egress filter, on ports 80 and 443 plus any `host:port` its `egress_allow` names. Where
  the filter is a container (colima, Docker Desktop, rootless or remote docker) it refuses, whatever
  the policy says, every docker network's gateway, the default bridge's subnet, its own routes'
  gateways, and the `/24` around what `host.docker.internal`, `host.lima.internal` and
  `gateway.docker.internal` resolve to. That closes the VM and, through it, your Mac's loopback.
  A docker network created after the filter started is refused once `sbx serve` has run a
  discovery pass (every `--refresh`, 15 s by default): the daemon lists the engine's gateways and
  pushes them to each container filter over its token-guarded control port, and the filter keeps
  the last push across a restart. With no daemon running, or with the filter on a remote docker
  (whose control port the daemon cannot reach), a network created later stays reachable on ports
  80 and 443 of its gateway until a daemon runs or the sandbox is removed and created again.
- The filter's control port (`sbx-egress:20998`) is reachable from the workload, so every path
  on it, the activity reading included, needs the per-filter token the daemon holds.
- A spec is executable: it names images, commands (`health`, `init`) and host files to mount.
  Treat someone else's `sandbox.json` like their Makefile.
- `${VAR}` keeps a secret out of a committed spec, but the value still reaches the container's
  environment, where `docker inspect` can read it.

### MicroVMs (`--provider firecracker`)

Each service is a microVM with its own guest kernel. Its VMM runs jailed, with no capabilities, in
its own network namespace, behind a host guard that fails closed.

Network:

- Each sandbox is a bridge (`10.231.<slot>.0/24`, host at `.1`) with no NAT, so a guest has no
  route off the host.
- A filtered sandbox reaches the internet only through the egress filter on
  `10.231.<slot>.1:20999`.
- The filter refuses the host's addresses and loopback, other sandboxes' guests, private ranges
  (RFC 1918, CGNAT, ULA) and neighbours on a host interface's subnet. No sandbox policy can open
  them. Only the operator can open private ranges, with `sbx serve --vm-egress-allow <CIDR,...>`.
- The host guard closes the host to a guest except for the filter port. Per bridge it adds an
  INPUT chain `SBX-FC<slot>`, a mangle PREROUTING drop before docker's DNAT (so published ports and
  NodePorts are closed too), mangle FORWARD drops, and IPv6 off on the bridge.
- The guard is re-checked on every wake and daemon reconcile, and a flushed rule is put back.
  `sbx doctor` shows it (`vm host guard`).
- If `iptables` is missing, a rule is refused, IPv6 cannot be disabled or a guard cannot be put
  back, the create or wake is refused with the fix. A bridge already in use keeps its running VMs,
  logged, and starts no new one. An API sandbox's status message carries the same warning.
- `--fc-firewall=unmanaged` (`SBX_FC_FIREWALL=unmanaged`) makes sbx write no rules. Closing the
  host to `10.231.0.0/16` is then your job, in INPUT and anywhere (docker, kube-proxy) that
  rewrites a guest's packet past INPUT.
- Without the guard, isolation between sandboxes depends on the host's FORWARD policy. With
  `ip_forward=1`, bridges can reach each other unless that policy is `DROP`. `sbx doctor` checks it
  (`vm bridges isolated`), and every create warns when it is not confirmed.

The jail (on by default; `sbx doctor` shows `vm jailer`):

- Every VMM runs under Firecracker's jailer, from the same pinned v1.17.0 tarball as the VMM.
- Its chroot, `<vm dir>/jail/firecracker/<id>/root`, holds only its kernel and shared base root
  (root's, read-only), its own drives and snapshot files, and `/dev/kvm`, `/dev/net/tun`,
  `/dev/urandom`. The agent drive and read-only volumes stay root's.
- Its uid and gid are `900000 + slot*256 + index`: never 0, never shared by two VMs. The range must
  hold no real account (`SBX_FC_JAILER_UID_BASE` moves it); sbx does not check `/etc/passwd`.
- It has no capabilities (`CapEff` is 0, checked in CI) and its own cgroup v2: `cpu.max` from the
  spec, `memory.max` its memory plus 128 MiB.
- Its network namespace holds only its tap, bridged to the guarded bridge with no address or route.
- Files a VMM writes are taken back only as plain files with one name, so it cannot plant a link
  for the host to follow. Its sockets are reached through sbx's own links, must be sockets owned by
  the jail's user, and on Linux must answer as that user (`SO_PEERCRED`).
- Each VM's record says how its VMM was launched (`jail_uid`), and later calls use that mode.

What remains open:

- `SBX_FC_JAILER=off` runs the VMM as unconfined root, so a guest that escapes into it has the
  host. It warns on every use, and the OpenSandbox API refuses it without `--osb-insecure-no-jailer`.
- `sbx serve --provider firecracker` runs as root (or with `CAP_NET_ADMIN`), because every create
  and wake does privileged work. A system unit can bound it with `CapabilityBoundingSet=`
  (`CAP_NET_ADMIN`, `CAP_NET_RAW`, `CAP_SYS_ADMIN`, `CAP_SYS_CHROOT`, `CAP_MKNOD`, `CAP_SETUID`,
  `CAP_SETGID`, `CAP_CHOWN`, `CAP_FOWNER`, `CAP_DAC_OVERRIDE`, `CAP_KILL`, `CAP_SYS_RESOURCE`). CI
  runs it as full root, so a bounded daemon is not tested.
- Each VM's disk is bounded: its writable layer (`SBX_FC_DISK_SIZE`, 10G), its volumes
  (`SBX_FC_VOLUME_SIZE`) and a memory file as big as its RAM. A jailed VMM's files are capped by
  `RLIMIT_FSIZE`, but not their number. Many VMs can still fill `SBX_FC_STATE`, which on `/` is the
  host. Put it on its own filesystem or enable project quotas; `sbx doctor` warns.
- A compromised VMM can refuse to answer, or answer its own API wrongly, about itself only.
- Inside a microVM, the guest's root can read execd's token and control secret (`/proc/1/environ`,
  `/init.json`). Each belongs to that guest alone, control is vsock-only from the host, the secret
  rotates at every restore and snapshot, and memory forks are refused.
- Snapshots taken before v0.13 cold-boot once (memory lost, disk kept).

## Access and exposure

- There is no authentication, per-user isolation or quota. Anyone who can reach the daemon's ports
  can use any sandbox, and anyone who can run `sbx` can destroy any of them. This will not change
  ([DECISIONS.md](docs/DECISIONS.md#sbx-is-a-tool-people-run-not-a-service-anyone-offers)).
- Ports bind to `127.0.0.1`. Only two opt-in features reach the network, and both say so:
  `sbx url` (one HTTP service, prints its URL) and `sbx serve --connect-addr`.
- `--connect-addr` refuses to start without `SBX_CONNECT_TOKEN`, and refuses a non-loopback
  address without `--behind-proxy`. Sandbox ports stay on loopback behind it.
- The connect token is the whole boundary. It gives TCP access to every service in the deployment,
  and wake, sleep, re-limit, remove and logs (`/v1/control/*`). It cannot `create` or `exec`.
- A bare `--front 5432` reaches only the daemon's own loopback. Naming a host (`--front host:port`)
  lets the token reach anything that container can route to, such as a private database. Front
  only the ports you need, and treat the token like a VPN credential.
- The OpenSandbox API always requires a key, loopback included, because on colima and Docker
  Desktop every container can reach the host's `127.0.0.1`. Set `--osb-key` or `SBX_OSB_KEY`, or
  one is generated into `~/.sbx/osb/key` (0600). `--osb-insecure-no-key` is loopback only.
- `sbx mcp` holds the API key, so an agent driving it can create, exec in and delete API
  sandboxes. Use `--url` only over a transport you trust.

## What would be a real vulnerability

Anything that breaks a boundary sbx claims to hold:

- A sandbox reaching another sandbox's data, or a fork inheriting state it should not.
- An OpenSandbox `host` volume binding anything but the directory validated under
  `--osb-host-paths`, at create or any later start
  ([DECISIONS.md](docs/DECISIONS.md#volumes-on-the-api-host-paths-are-the-operators-to-allow-and-claims-are-namespaced)).
- A container reaching the OpenSandbox API without the key, or getting a credential that works
  beyond its own execd.
- `egress: "deny"` permitting routed egress on docker.
- A filtered service reaching a destination its policy denies, directly, through the filter or a
  name that resolves into a denied range, or rewriting its own policy.
- `--isolation gvisor|kata` reporting success under the default runtime.
- A public port serving a different sandbox than `sbx env` named, including through a `sbx connect`
  tunnel to a recreated sandbox.
- `sbx gc` deleting an artifact of a live sandbox.
- Reaching the connect endpoint or `/v1/control/*` without the token, or it listening off loopback
  without `--behind-proxy`.
- On firecracker with the jailer on: a VMM with any capability, as uid 0 or another VM's uid, or
  writing outside its jail; a guest reaching the host, another guest or a private range past the
  filter and guard; a create or wake succeeding without the guard when the firewall is managed; two
  VMs sharing execd's token or control secret.
- A spec value reaching a shell. Values are passed as arguments, not interpolated.
- Path traversal out of `~/.sbx`, or a name that escapes the container, volume or image name it
  becomes.

## Advisories

### v0.15.1 and earlier: the egress filter reaches the host on a VM-backed engine (fixed in v0.16.0)

Affected: every version with a container egress filter, on colima and Docker Desktop, for a
service with `egress: "allow"`, or an `egress_allow`/`egress_policy` that allowed the addresses
below.

- The filter runs as a container on the engine's VM. Through it, a workload could `CONNECT` to
  `host.lima.internal`, `host.docker.internal` or `192.168.5.2`, which the VM forwards to the
  Mac's own loopback - every service bound to `127.0.0.1` there, including other sandboxes'
  published ports - and to the bridge gateway, which reached the VM's sshd.
- `CONNECT` also tunnelled any port, so an allowed name opened raw TCP to that host, not only
  HTTP and HTTPS.
- A workload with no egress filter was not affected: it has no route to any of these.
- Fixed in v0.16.0: the filter refuses every gateway of its networks, the default bridge's subnet
  and the `/24` around each host alias, re-read every 30 s, whatever the policy says; and it
  carries only ports 80 and 443 unless an `egress_allow` entry names another as `host:port`.
- Also fixed in v0.16.0: a docker network created after the filter started (another sandbox's)
  was not refused, and `CONNECT <its gateway>:443` reached the VM. `sbx serve` now pushes the
  engine's gateways to every container filter on each discovery pass. That needs the daemon
  running: a network created while none runs is reachable until one does.
- The filter's activity endpoint, `GET sbx-egress:20998/last`, answered the workload. It needs
  the control token since v0.16.0.
- Upgrading is not enough for an existing sandbox: run `sbx create` again over it (with v0.16.0 it
  replaces a filter built by an older sbx), or `sbx rm` and create it.
- Workaround before upgrading: use `egress: "deny"`, or an allow-list, for any sandbox that runs
  code you do not trust.

### v0.9.0: a keyless OpenSandbox API is reachable from every container (fixed in v0.9.1)

Affected: v0.9.0, only when `sbx serve --osb-addr` ran without `--osb-key` or `SBX_OSB_KEY`.

- v0.9.0 treated `127.0.0.1` as private. On colima and Docker Desktop, every container reaches the
  host's loopback, and the connection arrives from `127.0.0.1`.
- Any container on that engine could call the keyless API, read every sandbox's execd token, run
  commands and read files in any other API sandbox, create and delete sandboxes, and lift its own
  egress policy.
- No host escape: v0.9.0's API accepts no volumes, capabilities or privileged mode.
- Fixed in v0.9.1: a key is always required; the egress sidecar has its own credential outside the
  container; a non-loopback `--osb-addr` is refused; API pauses apply before any listener binds;
  API sandboxes carry an `sbx.osb` label (recreate sandboxes made by v0.9.0 to get it).
- Workaround on v0.9.0: pass `--osb-key` with a random value and give it to your clients. Treat
  sandboxes that ran untrusted code on a keyless v0.9.0 API as able to have touched every other
  API sandbox on that engine.

Why loopback is not a trust boundary:
[DECISIONS.md](docs/DECISIONS.md#loopback-is-not-a-trust-boundary-on-a-vm-backed-engine).
