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
- `sbx snapshot <sandbox> <name>` refuses a name that already exists instead of overwriting and merging into it. Use `--replace` to remove the old snapshot whole and take a fresh one.
- `cap_add: ["ALL"]` is refused; it granted every capability. List the ones the workload needs.
- `$${` in an `env` value is a literal `${`. v0.15 expanded `$${X}` to `$` followed by the value.

## Added

- `sbx gc` lists lock files whose holder is gone, and `--force` removes them.
- `sbx snapshot --rm <name>` deletes one snapshot's images and volumes, and refuses while a fork still uses it.
- An `egress_allow` entry written as `host:port` allows exactly that port for that host and its subdomains.
- `sbx env` prints `<SERVICE>_HOST` and `<SERVICE>_PORT` for services no export names, such as ones added with `sbx add`.
- `sbx list` shows a service paused by `on_idle: "freeze"` as `frozen`; `--json` adds a `state` field.
- `sbx pack --version vX.Y.Z` pins the release the packed image installs; a source build needs it.
- `sbx list` shows each service's isolation tier: an ISOLATION column, and `isolation` in `--json`.
- `sbx create` warns when a new sandbox starts on a data volume an earlier sandbox of the same name left behind.
- `sbx init --from-devcontainer` translates `${localEnv:X}` and `${containerWorkspaceFolder}`, escapes every other `${`, and lists what it could not evaluate.

## Changed

- Every command accepts flags before, between or after the names (`sbx logs -f b svc`), and a bare `--` makes the rest names.
- `sbx list` has a new ISOLATION column before ADDRESS; a script reading the table by column position should use `sbx list --json`.
- `sbx create` run again recreates a service whose image changed (an edited `build` context, or a new tag), keeping its volume; on kubernetes it patches the deployment's image.
- `sbx add` puts the new service on the sandbox's own isolation tier; a different `--isolation` is refused.
- `sbx exec` passes piped stdin to the command and exits with its status.
- `sbx snapshot` saves a service without a `volume` as its image only, instead of failing.
- `sbx checkpoint` is refused up front on a local macOS engine, where a checkpoint could be taken but never resumed.
- `sbx validate` refuses an unknown or blank `cap_add` name, a `${VAR:-x}` or other non-plain `${...}` in `env` (every one in the file at once), and `cpu`, `memory` or `gpus` values no provider accepts. A `CAP_` prefix is accepted, as docker does.
- `sbx serve --osb-addr` on a source build compiles the sandbox agent at startup, so the first create does not time out, and warns at start when the agent cannot be found.
- `sbx ready`, `sbx wake` and `sbx create` check inside each container that something listens on its ports where outside can reach them, and that it has a network; the error names which is missing.
- `sbx sleep` stops frozen services too, and stops services in parallel, dependents before what they `depends_on`.
- `sbx create` stops with an error naming the holding pid if the slot or sandbox lock stays held for 10 minutes; it used to go ahead unlocked after 90 s. An API create in that case is `Failed` with `slot_lock_timeout`.
- `sbx with` refuses a name another `sbx create`, `add` or `with` is making, and its teardown removes only the containers it created.
- `sbx create` and `sbx add` on a name a running `sbx with` owns are refused at once instead of joining a sandbox that command will remove.
- `sbx install` exits non-zero when a name you gave cannot be installed, `--dry-run` included.
- Restart `sbx serve` after upgrading, and do not run two sbx versions at once: an older daemon cannot read a new egress filter's activity or snapshot pause marks, and lock files now store the holder's start time, which an older sbx cannot read.

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
- `sbx selftest` registers as an `--only` daemon for its own sandbox while it runs, so `sbx serve` leaves it alone instead of logging "address already in use".
- A service's own `idle` window is honoured within a third of it, not at the daemon's 30 s cadence.
- `sbx history` records sleeps from `sbx sleep` and wakes caused by `sbx exec` and `sbx cp`.
- After `sbx sleep`, the first connection to a service starts the services it `depends_on` as well; they stayed stopped until the next discovery tick and a new connection.
- `sbx env` no longer needs `${VAR}` secrets set, and `sbx add` keeps clear of the spec's reserved ordinals when one is unset.
- A `depends_on` cycle is reported at load as the loop itself, with the file name.
- `sbx validate` and every command that expands a spec report unsupported `${...}` forms and unset variables together in one error.
- A bare `PORT` export no longer sets `PORT_HOST`.
- `sbx gc` reports skipped snapshot images and volumes by kind, and too-new artifacts as a separate count.
- An API create on a source build no longer asks docker for an unpublished image, and a failed placement leaves no empty `sbx-execd-*` volume.
- A refused `sbx serve --osb-addr` no longer writes `~/.sbx/osb/key`.
- `sandbox_create` in `sbx mcp` suggests raising `ready_timeout_seconds` only when the wait timed out.
- `sbx doctor` says an isolation runtime is registered, not available, and notes Kata is unverified on nested hosts.
- Security: a docker network created after a sandbox's egress filter started is refused too; `sbx serve` pushes the engine's gateways to every container filter each discovery pass.
- The egress filter's activity endpoint needs its control token; a sandbox could read it.
- The egress filter refuses its own addresses, so `CONNECT sbx-egress:443` from a sandbox is refused instead of dialled back into the filter.
- A request to the egress filter itself (`sbx-egress`, its hostname or its container name) gets a 403 saying no policy opens it, instead of advice to add a `host:port` grant that could never work.
- A 403 for a port the filter does not carry no longer suggests a grant for an address no policy opens.
- Two `sbx with` of one name no longer share a sandbox and remove it under each other.
- `sbx with` removes its sandbox on Ctrl-C or SIGTERM, passes the signal to the command, and exits 130 or 143.
- Concurrent `sbx create`s no longer wait on each other's health checks or land on one slot.
- A `docker run` that fails no longer leaves a `Created` container; a new sandbox whose ports were taken retries once on the next slot.
- A rebuild while asleep runs the new container's checks and `init` instead of calling it asleep.
- A failed mount check lists the services kept and those not attempted.
- `sbx with --keep` says the sandbox was kept; `sbx add` refuses a duplicate service before any `--health` warning.
- A lock, daemon record or snapshot pause mark whose pid was reused by another process no longer blocks a name or claims a daemon.
- A firecracker helper-VM lock whose pid was reused by another process no longer blocks `sbx fc` and firecracker creates for 15 minutes.
- `sbx env` and `sbx ready` find the spec of a sandbox whose create failed after making containers.
- Re-creating a sandbox from a different spec refuses a service a port its own old service holds, naming both, instead of failing in docker.
- `sbx with` no longer suggests `sbx rm` for a sandbox it already removed.
- `sbx snapshot --rm` and `--replace` refuse, removing nothing, while a fork still runs from the snapshot.
- `sbx gc --snapshots` no longer offers a snapshot any sandbox runs from; `--force` never removes one.
- `sbx snapshot --rm` also removes a snapshot that exists only as volumes, which is what an interrupted snapshot leaves; `sbx gc --snapshots` lists such volumes as "no image".
- `sbx snapshot` pauses the sandbox's running services for the copy and commit, then thaws them (also on failure and Ctrl-C), so a database rewriting its files, such as ClickHouse merging parts, is saved at one instant instead of failing with "can't stat" or tearing.
- `sbx prewarm` also pulls the helpers a spec needs (`alpine:3` for snapshot and fork, the egress filter's images when a service is filtered) and lists each image once, even one named twice on the command line.
- A service with a short `idle` sleeps on time when another is slow to stop, and a connection during a stop can no longer leave it running while the daemon believes it asleep. The `slept` event reports the idle time that triggered it.
- `sbx env` warns on stderr when a derived `<SERVICE>_PORT` is taken by an export or shared by two services, and gives a shared name to neither.
- `sbx add` warns when the service it adds takes a `<SERVICE>_PORT` name another service or an export already has, and `sbx validate` and `sbx create` warn about such collisions in the spec itself; none of them refuses.
- A tier mismatch on `sbx add` names `SBX_ISOLATION` when that asked for it, and an unknown sandbox always lists the ones that exist.
- `sbx logs -f` says so when the followed service goes to sleep.
- `sbx install checkpoint` on macOS gives the real reason, a restore needs a Linux host.
- `sbx pack` on a source build suggests the release the build is based on.
- A `sbx serve` without `--only` leaves the sandboxes a running `--only` daemon covers to that daemon, and takes them back within one `--refresh` after it stops; both used to bind the same ports.
