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
  own addresses and every other sandbox's unless a rule names them. Two consequences:
  - **The host is closed to a guest except for that filter port** - where sbx could install it.
    Each bridge gets a chain `SBX-FC<slot>` in INPUT (replies returned to your rules, the filter
    port accepted, the rest dropped) and another in mangle PREROUTING that drops what a guest starts
    before docker's DNAT can turn it into forwarded traffic - so a docker-published port, a
    container's IP and a NodePort are closed too, not only services bound to the host - plus
    mangle FORWARD drops from and to the bridge, and IPv6 off, made with the bridge and removed with it
    (DECISIONS.md, "A microVM's only door is its filter"). **Where `iptables` is missing or refuses,
    the bridge still comes up and a guest reaches every host service bound to `0.0.0.0` at
    `10.231.<slot>.1`**; the create and the daemon's log say so. Bridges made by v0.11 are not
    guarded until the sandbox is recreated.
  - **Isolation between sandboxes is sbx's where the guard is installed** (the mangle FORWARD
    drops), and the host's FORWARD policy where it is not. With `ip_forward=1` (docker turns it on)
    an unguarded bridge's VMs can reach another's unless the policy is `DROP`. `sbx doctor` checks it (`vm bridges isolated`), and every
    create warns when it is not confirmed.
- **In a microVM, the guest's root can read execd's own secrets** - the access token and the
  boot control secret, from `/proc/1/environ` and `/init.json` on the agent drive. execd strips
  them from what it starts, which keeps them out of `env` and logs, not from root. They are
  harmless there by construction: each belongs to that guest alone, control is reachable only
  over vsock from the host, the control secret is rotated at every restore and every snapshot
  (the running VM and a saved one never share it), and forking a VM is refused.
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
