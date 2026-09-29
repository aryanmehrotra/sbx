# Unreleased

Changes merged since the last release, collected one line per pull request. At release time
they are folded into `vX.Y.Z.md` from TEMPLATE.md, and this file is reset to empty sections.
This file is never a release body itself.

How to add a line:

- Write for a user, not a reviewer: what they can now do, or the symptom that is fixed.
- One line, about 20 words, starting with the command, flag or field in code font when there is one.
- Put it under exactly one section. A behaviour change that needs action from a user is Breaking.
- No links, or absolute ones only: relative links break once this text becomes a release body.
- Internal changes (tests, CI, refactors, contributor and style docs) do not get a line. A new
  user guide does.
- A pull request that changes `main.go` or `internal/` either adds a line here or ticks "No:
  internal only" in the PR template; CI checks it (`scripts/check-unreleased.sh`).

Shapes: "`sbx COMMAND --FLAG` now does WHAT THE USER GETS." or "SYMPTOM no longer happens when CONDITION."

At release time, Breaking and Changed become the note's "Before you upgrade", Added becomes
"Highlights", and Fixed becomes "Fixes".

## Breaking

- `egress_allow`, `egress_policy` and `egress: "allow"` carry only ports 80 and 443; other ports get 403. Write `"host:port"` in `egress_allow` to allow another port.
- `sbx with <name>` refuses a name that already exists, instead of reusing that sandbox and then deleting it and its volumes. Use `sbx env` or `sbx exec` against an existing sandbox.

## Added

- `sbx snapshot --rm <name>` deletes one snapshot's images and volumes, and refuses while a fork still uses it.
- An `egress_allow` entry written as `host:port` allows exactly that port for that host and its subdomains.
- `sbx env` prints `<SERVICE>_HOST` and `<SERVICE>_PORT` for services no export names, such as ones added with `sbx add`.
- `sbx list` shows a service paused by `on_idle: "freeze"` as `frozen`; `--json` adds a `state` field.
- `sbx pack --version vX.Y.Z` pins the release the packed image installs; a source build needs it.

## Changed

- Every command accepts flags before, between or after the names (`sbx logs -f b svc`), and a bare `--` makes the rest names.
- `sbx create` run again recreates a service whose image changed (an edited `build` context, or a new tag), keeping its volume; on kubernetes it patches the deployment's image.
- `sbx add` puts the new service on the sandbox's own isolation tier; a different `--isolation` is refused.
- `sbx exec` passes piped stdin to the command and exits with its status.
- `sbx snapshot` saves a service without a `volume` as its image only, instead of failing.
- `sbx checkpoint` is refused up front on a local macOS engine, where a checkpoint could be taken but never resumed.
- `sbx validate` refuses an unknown `cap_add` name, a `CAP_`-prefixed one, a `${VAR:-x}` or other non-plain `${...}` in `env`, and `cpu`, `memory` or `gpus` values no provider accepts.
- `sbx serve --osb-addr` on a source build compiles the sandbox agent at startup, so the first create does not time out.

## Fixed

- Security: on colima and Docker Desktop, the egress filter no longer lets a sandbox reach the VM, other containers on the default bridge, or your Mac's loopback through `host.docker.internal` or `host.lima.internal`.
- `CONNECT` through the egress filter no longer tunnels raw TCP to any port.
- `sbx create` run again after editing `egress_allow` or `egress_policy`, or with a newer sbx, replaces the sandbox's egress filter instead of keeping the old one.
- `egress_allow` works under `--isolation gvisor`; it used to fail with `bad address 'sbx-egress:20999'`.
- Sandboxes whose services each list their own `egress_allow` hosts no longer lose the first service's hosts.
- `sbx egress --remove` of a target with no rule is an error listing the rules, instead of exiting 0.
- `sbx with` removes a sandbox whose create failed partway, and `--timeout` bounds each health wait instead of a fixed 2 minutes.
- A service whose file or mount check fails at create has its container removed instead of left serving the broken mount.
- `sbx ready` and `sbx wake` no longer report "serving" for a container that exited, or when docker, Kubernetes or a microVM could not be asked.
- `sbx rm x` then `sbx create x` within one refresh no longer leaves the daemon serving the old ports.
- `sbx create` re-run over an asleep sandbox no longer reports a `files` entry as "did not mount as a file".
- A failed `sbx snapshot` no longer leaves images or volumes behind while saying "nothing was changed".
- `sbx resume` refuses a running service instead of reporting "memory and processes intact".
- `sbx fork` writes `sandbox.<fork>.json`, so forks of one snapshot no longer share a spec file, and forks of `build` services work.
- `sbx selftest` no longer fronts or sleeps other sandboxes on the machine, and its log no longer interleaves with its output.
- A service's own `idle` window is honoured within a third of it, not at the daemon's 30 s cadence.
- `sbx history` records sleeps from `sbx sleep` and wakes caused by `sbx exec` and `sbx cp`.
- `sbx env` no longer needs `${VAR}` secrets set, and `sbx add` keeps clear of the spec's reserved ordinals when one is unset.
- A `depends_on` cycle is reported at load as the loop itself, with the file name.
- A bare `PORT` export no longer sets `PORT_HOST`.
- `sbx gc` reports snapshots and too-new artifacts as separate counts.
- An API create on a source build no longer asks docker for an unpublished image, and a failed placement leaves no empty `sbx-execd-*` volume.
- A refused `sbx serve --osb-addr` no longer writes `~/.sbx/osb/key`.
- `sandbox_create` in `sbx mcp` suggests raising `ready_timeout_seconds` only when the wait timed out.
- `sbx doctor` says an isolation runtime is registered, not available, and notes Kata is unverified on nested hosts.
