# Contributing

Issues and patches are welcome. Every rule for code and docs lives in [AGENTS.md](AGENTS.md);
read it before your first PR. This page covers setup, test tiers, pull requests and releases.

## Quick start

You need Go (version in `go.mod`) and, for most tiers, Docker. There are no Go dependencies to
fetch, and there must stay none ([hard rule 1](AGENTS.md#hard-rules)).

```sh
go build -o sbx . && ./sbx doctor   # what your machine can do
go test -short ./...                # unit tests, no Docker
```

The full build, vet and docs-lint list is in [AGENTS.md](AGENTS.md#build-and-test).

## Test tiers

Run the tier that covers what you changed. CI runs all of them (`.github/workflows/ci.yaml`).

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

- `-short` skips tests that start real containers, which cost about a minute.
- `platforms.sh` vets as well as builds, because only vet type-checks `_test.go` files.
- `linux-tests.sh` matters on a Mac: the bridge gateway, CRIU and microVM paths are Linux-only.
- The microVM e2e tests pass a linux `sbx` as `SBX_EXECD_BINARY`. Copy the commands from the `microvm` job.
- CI's gVisor `isolation`, `connections` and `zeropod` jobs are measurements, not suites to run locally.

## Opening a pull request

1. Branch from `main`. The [repo map](AGENTS.md#repo-map) and [ARCHITECTURE.md](docs/ARCHITECTURE.md) help you find your way.
2. Add a test that fails without the change. Update the pages the [docs contract](AGENTS.md#docs-contract) names.
3. Run `go vet ./... && gofmt -l .`, `shellcheck -S warning` on scripts you touched, and the matching tier.
4. Fill in the PR template. CI reads its first box: tick "No: internal only" or add a line to `docs/release-notes/UNRELEASED.md`.
5. If something surprised you, append a dated line to AGENTS.md's [Lessons](AGENTS.md#lessons-append-dont-rewrite).

## Cutting a release

A release is a tag, and everything after it is automated. The release workflow refuses to build
without hand-written notes.

1. Write `docs/release-notes/vX.Y.Z.md` from `TEMPLATE.md` and the lines in `UNRELEASED.md`, then
   reset UNRELEASED.md. The `release-notes` skill in `.claude/skills/` is the checklist.
2. Commit any image the note uses first, so the tag-pinned link resolves. `scripts/ui-shot.sh` re-records the dashboard.
3. In the same commit, bump the version stamps: the index row in `docs/release-notes/README.md`,
   the supported version in `SECURITY.md`, the "at vX.Y.Z" line in
   [platform status](docs/ARCHITECTURE.md#platform-status), and ROADMAP's "As of" line and "Shipped recently".
4. Tag and push: `git tag -a vX.Y.Z && git push origin vX.Y.Z`. This builds every target,
   publishes binaries with `SHA256SUMS` and pushes the activator image.
5. The release gets a `bench.md` with the runner's figures. It does not edit
   [BENCHMARKS.md](docs/BENCHMARKS.md); refresh that by running its scripts on a machine you describe.
6. After step 4 publishes checksums, update the tap: `scripts/brew-formula.sh vX.Y.Z > ../homebrew-tap/Formula/sbx.rb`.

## Lessons archive

Lessons moved out of [AGENTS.md](AGENTS.md#lessons-append-dont-rewrite) past about 25 entries,
oldest first, unchanged. None yet.
