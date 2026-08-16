# Security

## Reporting

Report a vulnerability through [GitHub's private advisory
form](https://github.com/aryanmehrotra/sbx/security/advisories/new). Please don't open a
public issue for something exploitable.

Expect an acknowledgement within about a week. If a report is confirmed, the fix and the
advisory go out together.

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
- **A container shares the host kernel.** `--isolation gvisor|kata` asks for a stronger
  boundary and is *refused with a reason* where the runtime is absent rather than silently
  downgraded. If you are running code you did not write, use one of those or use a tool built
  on microVMs; [COMPARISON.md](docs/COMPARISON.md) names them.
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
- `egress: "deny"` permitting routed egress on docker.
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

Pre-1.0: fixes land on `main` and there is no backport branch. Use the latest tag.
