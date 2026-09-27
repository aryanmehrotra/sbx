# Contributing

Issues and patches are welcome. This page is for people changing sbx: how to build it, which
tests to run, what a pull request should include, and how a release is cut. Coding agents read
[AGENTS.md](AGENTS.md), which holds the same rules in a denser form plus the docs contract.

## Quick start

```sh
go build -o sbx .
./sbx doctor         # what your machine can and cannot do
go test -short ./... # unit tests, no docker needed
./sbx selftest       # the whole cycle end to end, ~9 s once images are local
```

Nothing to install beyond Go (see `go.mod` for the version). The root module uses only the
standard library, and CI fails if `go.mod` gains a `require` line. That is a product claim, not a
preference: `go install` has to stay a single step.

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
| shell, workflows, docs, pins | - | `scripts/lint-docs.sh`, `scripts/lint-workflows.sh`, `scripts/pin-templates.sh --check`, `scripts/lib/measure_test.sh`, `scripts/compare_test.sh`, `scripts/fc-jail-watch_test.sh` |

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

## What a pull request includes

The PR template (`.github/pull_request_template.md`) asks for each of these.

- **A test that fails without the change.** Write the test, break the code, confirm it goes red.
  Several tests here once passed for the wrong reason and had to be rewritten.
- **The docs it affects, in the same PR.** [AGENTS.md](AGENTS.md#docs-contract) maps each kind
  of change (command, flag, env var, spec field, error message, platform status) to the page
  that must change. The house style is [docs/STYLE.md](docs/STYLE.md).
- **One line in `docs/release-notes/UNRELEASED.md`** for anything a user would notice.
- **A measurement, if it claims to be faster.** Every number in
  [BENCHMARKS.md](docs/BENCHMARKS.md) names the script that produced it. Two rules:
  - **Interleave and alternate.** Running A then B in every round hands B a docker daemon that A
    has just finished hammering. That bias once reversed the sign of a result here.
  - **A delta inside the run-to-run spread is not a result.** Say it was not resolvable. A change
    that is less work in theory but measures as nothing is a fine change; describe it that way.
- **Vendor claims quoted from the vendor, with a link.** `scripts/lint-docs.sh` checks that every
  external URL resolves. It cannot check that the page still says what we attribute to it, so a
  reviewer opens the link. Date the claim.
- **A lesson, if you learned one.** Append one dated line to the "Lessons" section of
  [AGENTS.md](AGENTS.md#lessons-append-dont-rewrite).

## Code style

Match the file you are editing. Two things are load-bearing:

- **Comments explain *why*, especially why the obvious thing was not done.** Most comments here
  exist because something broke. A comment that restates the code is noise.
- **Error messages say what to do next.** "never became ready" is a bad message. Naming the
  health command, its exit code and the one-liner that checks an image for the binary is a good
  one. Add the symptom to [TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md).

`gofmt`, `go vet` and `shellcheck -S warning` (on every script) are enforced in CI.

## Where things live

The full package map is in [AGENTS.md](AGENTS.md#repo-map); the design is in
[ARCHITECTURE.md](docs/ARCHITECTURE.md). The short version:

| Path | What |
|---|---|
| `main.go` | entry point only; calls `internal/app` |
| `internal/app/` | command dispatch, help text, MCP tools |
| `internal/cli/` | what each command does, provider-agnostic |
| `internal/daemon/` | `sbx serve`: wake/sleep state machine, byte proxy, egress, OpenSandbox front |
| `internal/provider/` | docker, kubernetes and firecracker backends |
| `internal/fc*`, `internal/osb`, `internal/execd`, `internal/egress`, `internal/mcp` | microVM, OpenSandbox API, in-sandbox agent, egress filter, MCP server |
| `internal/spec/` | `sandbox.json`: parsing, validation, port assignment |
| `docs/DECISIONS.md` | why it is shaped this way, mostly things that broke |

If you are about to change how a sandbox is addressed (ports, slots, labels), read
[ARCHITECTURE.md](docs/ARCHITECTURE.md) first. That scheme lives on every user's machine and is
the hardest thing here to change later.

## Cutting a release

A release is a tag; everything after it is automated. The notes are written by hand, before the
tag, and the release workflow refuses to build without them.

1. **Write `docs/release-notes/vX.Y.Z.md`.** Copy `docs/release-notes/TEMPLATE.md` and fold in
   the lines collected in `docs/release-notes/UNRELEASED.md`, then reset UNRELEASED.md to its
   empty sections. The reader is someone deciding whether to upgrade: what changed, why they
   would want it, how to adopt it, and what it costs them. Breaking changes go first. GitHub
   appends its generated commit list, so do not restate it. The `release-notes` skill in
   `.claude/skills/` walks through this.
2. **Use absolute links only.** The file becomes the GitHub release body, where relative links
   404. Pin links and images to the tag (`github.com/aryanmehrotra/sbx/blob/vX.Y.Z/...`,
   `raw.githubusercontent.com/aryanmehrotra/sbx/vX.Y.Z/...`, both over https), and commit any image first
   so it is in the tag. `scripts/ui-shot.sh` re-records the dashboard.
3. **Tag and push.** `git tag -a vX.Y.Z && git push origin vX.Y.Z`. That builds every target,
   publishes the binaries with `SHA256SUMS`, and pushes the activator image.
4. **The benchmarks run themselves.** A `bench.md` with the runner's own figures is attached to
   the release. It does not edit [BENCHMARKS.md](docs/BENCHMARKS.md), whose numbers come from a
   machine somebody can describe. To refresh those, run the scripts named there and say what you
   ran them on.
5. **Update the tap**, which is a separate repository:
   `scripts/brew-formula.sh vX.Y.Z > ../homebrew-tap/Formula/sbx.rb`. It reads the checksums from
   the published release, so wait for step 3.
