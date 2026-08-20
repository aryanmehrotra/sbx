# Contributing

Issues and patches are welcome. This file exists mainly to save you the twenty minutes it
would otherwise take to work out how the tests are arranged, because they are not all the
same kind of test and `go test ./...` is not the whole story.

## Getting it running

```sh
go build -o sbx .
./sbx doctor      # what your machine can and cannot do
./sbx selftest    # the whole cycle end to end, ~9 s once images are local
```

No dependencies to install: the root module uses nothing outside Go's standard library, and
CI fails if `go.mod` ever gains a `require` line. That is a product claim, not a preference -
`go install` has to stay a single step.

## The test tiers

| | what it needs | run it |
|---|---|---|
| unit | nothing | `go test -short ./...` |
| unit + docker-backed | a docker daemon | `go test ./...` |
| the daemon, end to end | docker | `./scripts/e2e.sh 3` |
| snapshot and fork | docker | `./scripts/fork-e2e.sh` |
| data safety under interruption | docker | `./scripts/interrupt-e2e.sh` |
| crash recovery | docker | `./scripts/recovery.sh` |
| endurance / leaks (on main push & release) | docker + a running `sbx serve` | `./scripts/soak.sh` |
| every documented use case | docker | `./scripts/usecases-e2e.sh` |
| shell, workflows, docs, pins | - | `scripts/lib/measure_test.sh`, `scripts/lint-workflows.sh`, `scripts/lint-docs.sh`, `scripts/pin-templates.sh --check` |

`-short` skips the tests that start real containers. They cost about a minute, which is worth
it in CI and not worth it on every local run.

`scripts/usecases-e2e.sh` takes a filter: `./scripts/usecases-e2e.sh build` runs only cases
whose name contains "build".

## What a change is expected to come with

**A test that fails without it.** Not as a formality - the convention here is to write the
test, break the code, and confirm the test goes red. Several tests in this repo passed on the
first attempt for the wrong reason and had to be rewritten; the commit messages say so where
it happened.

**A measurement, if it claims to be faster.** Every number in
[BENCHMARKS.md](docs/BENCHMARKS.md) names the script that produced it. Two rules are worth
knowing before you measure anything:

- **Interleave and alternate.** Running A then B in every round hands B a docker daemon that A
  has just finished hammering. That bias was once large enough here to reverse the sign of a
  result.
- **A delta inside the run-to-run spread is not a result.** Say it was not resolvable rather
  than publishing it. A change that is theoretically less work but measures as nothing is a
  fine change - describe it that way.

**A vendor claim, quoted from the vendor with a link.** `scripts/lint-docs.sh` checks that
every external URL in the docs resolves, because this project has published invented figures
about competitors five times and corrected them all in
[COMPARISON.md](docs/COMPARISON.md). Do not add a sixth.

## Style

Match the file you are editing. Two things are genuinely load-bearing:

- **Comments explain *why*, and especially why the obvious thing was not done.** Most of the
  comments here exist because something broke; a comment that restates the code is noise.
- **Error messages say what to do next.** "never became ready" is a bad message; naming the
  health command, its exit code and the one-liner that checks an image for a binary is a good
  one. There is a file of those in [TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md).

`gofmt` and `go vet` are enforced, as is `shellcheck -S warning` on every script.

## Cutting a release

A release is a tag, and everything else happens by itself - but the notes are written by hand,
before the tag, and the workflow refuses to build without them.

1. **Write `docs/release-notes/vX.Y.Z.md`.** Follow the last one. It is for somebody deciding
   whether to adopt this, not for somebody who already knows the codebase: what changed, why
   they would want it, the commands to adopt it, and what it costs them. GitHub's generated
   commit list is appended underneath automatically, so do not restate it.
2. **Commit any image it references first.** Release notes are rendered outside the repository,
   so pictures need absolute `raw.githubusercontent.com` URLs pinned to the tag - which means
   the file has to be in the tag. `scripts/ui-shot.sh` re-records the dashboard.
3. **Tag and push.** `git tag -a vX.Y.Z && git push origin vX.Y.Z`. That builds every target,
   publishes the binaries with `SHA256SUMS`, and pushes the activator image.
4. **The benchmarks run themselves.** A `bench.md` is attached to the release with the runner's
   own figures, for comparing against the previous tag. It does not edit
   [BENCHMARKS.md](docs/BENCHMARKS.md), whose numbers were measured on a machine somebody can
   describe - if you want those refreshed, run the scripts named there and say what you ran it on.
5. **Update the tap**, which is a different repository and so is not automated:
   `scripts/brew-formula.sh vX.Y.Z > ../homebrew-tap/Formula/sbx.rb`. It reads the checksums
   out of the published release, so there is nothing to read until step 3 finishes.

## Where things live

| | |
|---|---|
| `main.go` | command dispatch and flags |
| `internal/cli/` | what each command does, provider-agnostic |
| `internal/daemon/` | the wake/sleep state machine and the byte proxy |
| `internal/provider/` | docker and kubernetes, plus the optional capabilities |
| `internal/spec/` | `sandbox.json` - parsing, validation, port assignment |
| `docs/DECISIONS.md` | why it is shaped this way, mostly things that broke |

If you are about to change how a sandbox is addressed - ports, slots, labels - read
[ARCHITECTURE.md](docs/ARCHITECTURE.md) first. That scheme lives on every user's machine and
is the hardest thing here to change later.
