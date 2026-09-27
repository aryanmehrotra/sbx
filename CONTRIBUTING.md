# Contributing

Issues and patches are welcome. This page is for people changing sbx: how to set up, which
tests to run, how to open a pull request, and how a release is cut. Every rule (code, docs,
writing style, what must change with what) lives in [AGENTS.md](AGENTS.md); read it before your
first PR. Coding agents load it automatically.

## Quick start

```sh
go build -o sbx .
./sbx doctor         # what your machine can and cannot do
go test -short ./... # unit tests, no docker needed
./sbx selftest       # the whole cycle end to end, ~9 s once images are local
```

Nothing to install beyond Go (see `go.mod` for the version); for most tiers below, docker too.
There are no Go dependencies to fetch, and there must stay none
([hard rule 1](AGENTS.md#hard-rules)).

## Test tiers

`go test ./...` is not the whole story. Run the tier that covers what you changed; CI runs all of
them (`.github/workflows/ci.yaml`).

| Tier | Needs | Run it |
|---|---|---|
| unit | nothing | `go test -short ./...` |
| unit + docker-backed | docker | `go test ./...` |
| the daemon, end to end | docker | `./scripts/e2e.sh 3` |
| commands the journeys skip | docker | `./scripts/commands-e2e.sh` |
| `sbx connect` | docker | `./scripts/connect-e2e.sh` |
| snapshot and fork | docker | `./scripts/fork-e2e.sh` |
| data safety under interruption | docker | `./scripts/interrupt-e2e.sh` |
| crash recovery | docker | `./scripts/recovery.sh` |
| every documented use case | docker | `./scripts/usecases-e2e.sh [filter]` |
| endurance and leaks (main and release) | docker + a running `sbx serve` | `./scripts/soak.sh` |
| OpenSandbox compatibility (upstream's own suite) | docker, no other sbx sandboxes, network once | `./scripts/osb-conformance.sh`, see [test/osb](test/osb/README.md) |
| OpenSandbox use cases (MCP, interpreter, freeze, egress, fork, PTY...) | docker, no other sbx sandboxes | `./scripts/osb-usecases-e2e.sh [filter]` |
| microVM: jailer, no KVM | Linux, root, cgroup v2 | `go test -c -o fcjail.test ./internal/fc && sudo env SBX_FC_JAILER_E2E=1 ./fcjail.test -test.run TestTheRealJailer` |
| microVM: bridge guard, live | Linux, root | `go test -c -o fcguard.test ./internal/fc && sudo env SBX_GUARD_LIVE=1 ./fcguard.test -test.run '^TestGuardLive$'` |
| microVM: a real Firecracker VM | Linux, root, `/dev/kvm` | see the `microvm` job in `ci.yaml` (`SBX_FC_E2E=1`, `TestFirecrackerE2E`) |
| microVM through the helper VM | macOS on Apple M3+, lima or colima | `./scripts/fc-anywhere-e2e.sh` |
| every platform builds and vets | - | `./scripts/platforms.sh` |
| the suite on Linux, from a Mac | docker + an sbx sandbox | `SBX_SANDBOX=<name> ./scripts/linux-tests.sh` |
| shell, workflows, docs, pins | - | `scripts/lint-docs.sh`, `scripts/lint-docs-contract.sh`, `scripts/lint-workflows.sh`, `scripts/pin-templates.sh --check`, `scripts/lib/measure_test.sh`, `scripts/compare_test.sh`, `scripts/fc-jail-watch_test.sh` |

Notes:

- `-short` skips tests that start real containers. They cost about a minute.
- `usecases-e2e.sh build` runs only cases whose name contains "build".
- `platforms.sh` vets as well as builds all eight GOOS/GOARCH pairs, because vet type-checks
  `_test.go` files and build does not. That is how a test suite that did not compile on Windows
  was found.
- `linux-tests.sh` runs vet and the race suite inside an sbx sandbox on linux/arm64. It matters
  on a Mac: the daemon's bridge gateway, CRIU and the kernel-level microVM paths are Linux-only.
- The microVM e2e tests build `sbx` for linux and pass it as `SBX_EXECD_BINARY`; copy the exact
  commands from the `microvm` job rather than guessing them.
- CI also runs a gVisor `isolation` job, a `connections` benchmark and a `zeropod` probe; those
  are measurements or runtime checks, not suites you need locally.

## Opening a pull request

1. Branch from `main`. Find your way around with the [repo map](AGENTS.md#repo-map) and
   [ARCHITECTURE.md](docs/ARCHITECTURE.md).
2. Make the change with a test that fails without it, and update the pages the
   [docs contract](AGENTS.md#docs-contract) names for your kind of change.
3. Run `go vet ./... && gofmt -l .`, `shellcheck -S warning` on any script you touched, and the
   test tier above that covers the change. CI runs the rest.
4. Open the PR and fill in the template (`.github/pull_request_template.md`). Its first box,
   "No: internal only", is read by CI: tick it, or add a line to
   `docs/release-notes/UNRELEASED.md`.
5. If something surprised you, append a dated line to AGENTS.md's
   [Lessons](AGENTS.md#lessons-append-dont-rewrite).

## Cutting a release

A release is a tag; everything after it is automated. The notes are written by hand, before the
tag, and the release workflow refuses to build without them.

1. **Write `docs/release-notes/vX.Y.Z.md`.** Copy `docs/release-notes/TEMPLATE.md` (its header
   holds the format rules) and fold in the lines collected in `docs/release-notes/UNRELEASED.md`,
   then reset UNRELEASED.md to its empty sections. The `release-notes` skill in `.claude/skills/`
   walks through this; a person can read its `SKILL.md` as a checklist.
2. **Commit any image the note uses first**, so it is in the tag; links in the note are absolute
   and pinned to it. `scripts/ui-shot.sh` re-records the dashboard.
3. **Bump the version stamps**, in the same commit as the note. Each names the current release
   and goes stale at the next tag: the row in `docs/release-notes/README.md` (the index;
   `scripts/lint-docs-contract.sh` fails without it), the supported version in `SECURITY.md`, the
   "at vX.Y.Z" line in README's "Platform status", and ROADMAP's "As of" line and "Shipped
   recently" list.
4. **Tag and push.** `git tag -a vX.Y.Z && git push origin vX.Y.Z`. That builds every target,
   publishes the binaries with `SHA256SUMS`, and pushes the activator image.
5. **The benchmarks run themselves.** A `bench.md` with the runner's own figures is attached to
   the release. It does not edit [BENCHMARKS.md](docs/BENCHMARKS.md), whose numbers come from a
   machine somebody can describe. To refresh those, run the scripts named there and say what you
   ran them on.
6. **Update the tap**, which is a separate repository:
   `scripts/brew-formula.sh vX.Y.Z > ../homebrew-tap/Formula/sbx.rb`. It reads the checksums from
   the published release, so wait for step 4.

## Lessons archive

Lessons moved out of [AGENTS.md](AGENTS.md#lessons-append-dont-rewrite) once it passes about 25
entries, oldest first, unchanged. None yet.
