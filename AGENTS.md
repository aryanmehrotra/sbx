# AGENTS.md

Instructions for anyone changing this repository: coding agents (Codex, Cursor, Copilot, Claude
Code via `CLAUDE.md`) and human contributors. This file is the single source of the rules and of
what must change with what. [CONTRIBUTING.md](CONTRIBUTING.md) covers how to build, test and
release; [docs/STYLE.md](docs/STYLE.md) covers how to write. Both link here rather than restate
the rules. Using sbx *from* an AI agent is a different topic: see
[docs/AI-AGENTS.md](docs/AI-AGENTS.md).

## The project in three lines

- sbx gives every branch, task or AI agent its own self-hosted sandbox: a real Postgres, Redis or
  browser that sleeps at 0 B and wakes when anything connects. No SDK, no account, one binary.
- The first TCP connection is held, not refused, while the service wakes; unmodified clients work.
- Backends: docker, kubernetes, Firecracker microVMs. It also serves the OpenSandbox API and an MCP server.

## Repo map

| Path | What lives there |
|---|---|
| `main.go` | Entry point only: embeds `examples/`, stamps `version`, calls `app.Main`. Add nothing here. |
| `internal/app/` | Command dispatch (`app.go`, one `case` per command), help text (`help.go`), `sbx mcp` wiring (`mcp.go`). |
| `internal/cli/` | What each command does, provider-agnostic (create, snapshot, fork, doctor, gc, prewarm...). |
| `internal/daemon/` | `sbx serve`: wake/sleep state machine, byte proxy, freeze, egress proxy, `sbx connect` server, OpenSandbox front. Flags in `serve.go`. |
| `internal/provider/` | Backends: `docker_*.go`, `kubernetes*.go`, `firecracker*.go`, warm pool. |
| `internal/fc`, `internal/fchost`, `internal/fcvsock` | Firecracker: pinned VMM/kernel, jailer, rootfs, bridge guard; host detection and helper VM (`sbx fc`); vsock dialing. |
| `internal/osb/`, `internal/execd/`, `internal/execdctl/`, `internal/osbclient/` | OpenSandbox lifecycle API, in-sandbox execd agent, its host-side control, and a client. |
| `internal/mcp/` | MCP server (stdlib JSON-RPC). The tools are defined in `internal/mcp/tools.go`; `internal/app/mcp.go` only registers them. |
| `internal/egress/` | Egress filter: policy, rules, the in-sandbox filter, microVM refusals. |
| `internal/spec/` | `sandbox.json`: parsing, validation, port assignment. |
| `internal/tui`, `internal/ui`, `internal/tunnel`, `internal/ws*`, `internal/history`... | Dashboard, public links, WebSocket halves, audit history. Each package opens with a doc comment. |
| `scripts/` | e2e suites, benchmarks (`bench.sh`, `compare.sh`, `connbench.sh`), linters, release helpers. |
| `test/osb/` | OpenSandbox upstream conformance harness (its own Go module). |
| `console/` | Optional log console (its own Go module). |
| `deploy/`, `examples/` | Activator image and service files; built-in specs (embedded into the binary). |
| `docs/` | User docs, reference and explanation; `docs/design/` = design records; `docs/release-notes/` = one file per tag. |

Before changing how a sandbox is addressed (ports, slots, labels, names), read
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). That scheme lives on every user's machine.

## Build and test

```sh
go build -o sbx . && ./sbx doctor     # what this machine can do
go test -short ./...                  # unit, no docker (fast; run always)
go test ./...                         # + docker-backed tests (~1 min)
go vet ./... && gofmt -l .            # CI fails on either
./sbx selftest                        # whole cycle end to end, ~9 s with images local
bash scripts/lint-docs.sh             # docs links/anchors (SKIP_LINK_CHECK=1 offline)
bash scripts/platforms.sh             # all 8 GOOS/GOARCH build + vet (vet type-checks _test.go)
```

Heavier tiers (docker e2e, snapshot/fork, recovery, OpenSandbox conformance, microVM with
`/dev/kvm`) are listed in [CONTRIBUTING.md](CONTRIBUTING.md#test-tiers). Run the tier that covers
what you touched; CI runs all of them (`.github/workflows/ci.yaml`).

## Hard rules

1. **Zero Go dependencies.** The root `go.mod` has no `require`; CI enforces it. It is a product
   claim (`go install` is one step). Use the standard library.
2. **A test that fails without the change.** Write it, break the code, watch it go red. A test
   that passes on the first try may be passing for the wrong reason.
3. **Measurements** name the script, machine and version or date; A/B rounds are interleaved and
   alternated; a delta inside the spread is "not resolvable". Details: [docs/STYLE.md](docs/STYLE.md#numbers-and-sources).
4. **Vendor claims** are quoted from the vendor, linked and dated. No inferred figures.
5. **Error messages say what to do next**: the command, the value, the one-liner that checks it.
6. **Comments explain why**, especially why the obvious thing was not done.
7. **Do not change addressing** (ports, slots, labels) without reading ARCHITECTURE and DECISIONS.
8. **Do not rename headings others link to**, especially ROADMAP's "Not built yet in the
   OpenSandbox API" (runtime 501s and `ci.yaml` point at it). Run `grep -rn "FILE.md#" .` first;
   Go comments and `ci.yaml` cite doc paths too.
9. **Do not invent.** If code and a doc disagree, the code wins: fix the doc in the same PR.

## Docs contract

A PR that changes user-visible behaviour updates the docs in the same PR. Map:

| If the PR changes... | Update |
|---|---|
| A command, subcommand or flag | `internal/app/help.go` (tests check every command is explained), `docs/CLI.md` |
| A flag shared by several commands (`--spec`, `--template`, backend flags) | every command row in `docs/CLI.md` that takes it |
| A `sbx serve` flag or an `SBX_*` env var | `docs/CLI.md` |
| A feature gate (`internal/features`) | `docs/CLI.md` "Gated features" and the `sbx features` row |
| A `sandbox.json` field or its validation | [docs/SPEC.md](docs/SPEC.md); an `examples/` spec if it shows the feature |
| An error message or a new failure mode | [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md): symptom, cause, fix |
| A fix for a bug TROUBLESHOOTING gives a workaround for | remove or update that entry; name the fixed version |
| A headline feature, added or removed | README feature list (one line); [docs/USE-CASES.md](docs/USE-CASES.md) if it is a new shape |
| A built-in template (`examples/<name>/`) | `examples/README.md` table; README "Start from a template" row; SPEC.md's `sbx templates` line; `scripts/pin-templates.sh`; the `for t in` loop in `scripts/usecases-e2e.sh` |
| An MCP tool (`internal/mcp/tools.go`), or how agents should drive sbx | [docs/AI-AGENTS.md](docs/AI-AGENTS.md) (tool list); the tool count, which also appears in README, QUICKSTART, CLI and ARCHITECTURE (twice) |
| An OpenSandbox endpoint built or refused | ROADMAP's "not built" list (runtime errors point there), `test/osb/expectations` |
| Where something runs or is verified | README platform-status table (see "Status honesty" below) |
| A performance number | [docs/BENCHMARKS.md](docs/BENCHMARKS.md), with script, machine, version |
| A comparison with another tool | [docs/COMPARISON.md](docs/COMPARISON.md), with a dated vendor link |
| Wake, sleep, freeze or proxy behaviour | ARCHITECTURE.md's section for it; label a published figure that is now stale in BENCHMARKS and README "Measured on" (do not delete it) |
| A package, component or addressing | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md); a new entry in [docs/DECISIONS.md](docs/DECISIONS.md) for the why |
| A roadmap item that shipped | [docs/ROADMAP.md](docs/ROADMAP.md): remove it (the release note records it) |
| Anything a user would notice | one line in [docs/release-notes/UNRELEASED.md](docs/release-notes/UNRELEASED.md) |

"No docs needed" is a valid answer; say why in the PR description.

CI checks the rows a machine can check: every command and `sbx serve` flag has a row in
`docs/CLI.md` (`internal/app/clidoc_test.go`); every `SBX_*` variable the code reads is in
`docs/CLI.md`, and the ROADMAP heading and Lessons format hold (`scripts/lint-docs-contract.sh`);
a PR that changes `main.go` or `internal/` adds an UNRELEASED.md line or ticks "No: internal
only" (`scripts/check-unreleased.sh`). The rest is on the author and the reviewer.

### Status honesty

State what is proven, where. Use exactly four labels, defined in
[docs/STYLE.md](docs/STYLE.md#status-vocabulary): **verified in CI**, **unit-tested**, **run by
hand** (say where and at what version), **not yet run end to end**. Caveats about a platform or
backend live in one place, the README platform-status table, not in hero prose or feature rows.
When a CI job starts covering a path, update that table in the same PR.

### Page types (Diátaxis)

Each page is one of: **tutorial** (learn by doing), **how-to** (one task), **reference** (facts to
look up), **explanation** (why). History and rationale go to DECISIONS.md or release notes, never
into reference pages. The mapping of every page and the house style: [docs/STYLE.md](docs/STYLE.md).

### Doc skills

For agents that load skills: `.claude/skills/write-docs`, `.claude/skills/review-docs`,
`.claude/skills/release-notes`. Other agents: read the `SKILL.md` files directly; they are plain
Markdown checklists.

## Pull requests

Fill in `.github/pull_request_template.md`: what and why, the docs you touched (or why none),
the UNRELEASED.md line, the test that fails without the change, any measurement. Keep PRs small
enough to review; split refactors from behaviour changes.

## Lessons (append, don't rewrite)

Each PR that learns something the hard way appends **one dated line** here, in the form
`- YYYY-MM-DD · rule. Evidence (commit or file:line).` Do not edit or delete earlier lines; if one
is obsolete, append a line saying so. `scripts/lint-docs-contract.sh` checks the date format.
This file is loaded into every agent's context: past about 25 entries, move the oldest to
[CONTRIBUTING.md](CONTRIBUTING.md#lessons-archive).

- 2026-08-16 · Quote vendor figures with a link, never infer them. Invented ranges and a cold start
  quoted as a wake shipped in COMPARISON (a4a1c69, 229d696). `lint-docs.sh` checks that links
  resolve; it cannot check that the page still says the figure.
- 2026-08-16 · A reference-style link without a definition renders as a fake citation. Four sat in
  COMPARISON's vendor table (a4a1c69). `lint-docs.sh` now rejects them.
- 2026-08-16 · Check `#anchors`, not just files. A link to `README.md#use-it` (859b179) landed
  readers at the top of the page; fixed and linted in b92d839.
- 2026-08-16 · Animated SVGs are blank in static snapshots (social cards, PDFs, previews). Three
  versions of `docs/demo.svg` shipped that way. Draw lines; do not reveal them (fa1a76e). Linted.
- 2026-08-16 · Interleave A/B benchmark rounds. Running A then B read a change as 8-12% slower
  because B inherited a busy docker daemon; alternating, it was +3% (6925372).
- 2026-08-16 · `sbx --help` goes to stdout, usage errors to stderr. v0.1.0 had both on stderr and
  the Homebrew formula test caught it (6c93126, `internal/app/usage_test.go`).
- 2026-08-30 · `go vet` every platform, not just `go build`. Production code compiled on Windows
  while the test suite did not (d0ed349); `scripts/platforms.sh` now vets all eight targets.
- 2026-09-27 · Release notes become the GitHub release body, where relative links 404. Use
  absolute URLs pinned to the tag, including images (`raw.githubusercontent.com/.../vX.Y.Z/...`).
  Nine published notes shipped with relative links; linted since e5dfe12.
- 2026-09-27 · When a feature ships, fold it into every page's opening, not an appendix. The
  microVM provider (v0.11-v0.14) was appended, so page openings kept contradicting it.
- 2026-09-27 · Update ROADMAP in the PR that ships the item. The microVM provider shipped in
  v0.11-v0.14 while the old ROADMAP's microVM section still pitched it with an effort estimate (2b77953).
- 2026-09-27 · Do not promise a version for unbuilt work. API errors said "arrives in v0.11.0";
  v0.11.0 shipped without it and later notes had to retract it (fc34811). Say "not built yet (ROADMAP)".
- 2026-09-27 · Keep history out of reference docs. "Used to", "(v0.13)" and "an earlier draft"
  crept into SPEC, COMPARISON and ROADMAP; state the current behaviour once.
