# AGENTS.md

Rules for anyone changing this repository, people and coding agents alike (Claude Code reads it
via `CLAUDE.md`): the rules, the docs contract and the writing style. What sbx is:
[README.md](README.md). Setup, test tiers and releases: [CONTRIBUTING.md](CONTRIBUTING.md).

## Repo map

| Path | What lives there |
|---|---|
| `main.go` | Entry point only: embeds `examples/`, stamps `version`, calls `app.Main`. Add nothing here. |
| `internal/app/`, `internal/cli/` | Command dispatch (`app.go`, one `case` per command), help text (`help.go`), `sbx mcp` wiring; what each command does, provider-agnostic. |
| `internal/daemon/` | `sbx serve`: wake/sleep state machine, byte proxy, freeze, egress proxy, `sbx connect` server, OpenSandbox front. Flags in `serve.go`. |
| `internal/provider/`, `internal/fc*` | Backends (`docker_*.go`, `kubernetes*.go`, `firecracker*.go`, warm pool). Firecracker: pinned VMM/kernel, jailer, rootfs, bridge guard, helper VM (`sbx fc`), vsock. |
| `internal/osb/`, `internal/execd*/`, `internal/osbclient/` | OpenSandbox lifecycle API, in-sandbox execd agent, its host-side control, and a client. |
| `internal/mcp/`, `internal/spec/`, `internal/egress/` | MCP server (tools in `tools.go`; `internal/app/mcp.go` only registers them); `sandbox.json` parsing and validation; the egress filter. |
| `internal/tui`, `ui`, `tunnel`, `ws*`, `history`... | Dashboard, public links, WebSocket halves, audit history. Each package opens with a doc comment. |
| `scripts/`, `test/osb/`, `console/` | e2e suites, benchmarks, linters, release helpers; OpenSandbox conformance harness and log console (own Go modules). |
| `deploy/`, `examples/`, `docs/` | Activator image and service files; built-in specs (embedded); the [doc map](#doc-map) pages, `docs/design/` records, one release note per tag. |

## Build and test

```sh
go build -o sbx . && ./sbx doctor     # what this machine can do
go test -short ./...                  # unit, no docker (fast; run always)
go test ./...                         # + docker-backed tests (~1 min)
go vet ./... && gofmt -l .            # CI fails on either
./sbx selftest                        # whole cycle end to end (needs docker)
SKIP_LINK_CHECK=1 bash scripts/lint-docs.sh && bash scripts/lint-docs-contract.sh   # docs
bash scripts/platforms.sh             # all 8 GOOS/GOARCH build + vet (vet type-checks _test.go)
```

Heavier tiers (e2e, fork, recovery, conformance, microVM): [CONTRIBUTING.md](CONTRIBUTING.md#test-tiers). Run the one covering your change.

## Hard rules

1. Zero Go dependencies. The root `go.mod` has no `require`; CI enforces it. `go install` being one step is a product claim.
2. A test that fails without the change. Break the code and watch it go red.
3. Measurements name the script, machine and version: [Numbers and sources](#numbers-and-sources).
4. Vendor claims are quoted from the vendor, linked and dated. No inferred figures.
5. Error messages say what to do next: the command, the value, the one-liner that checks it.
6. Comments explain why, especially why the obvious thing was not done.
7. Do not change addressing (ports, slots, labels) without reading ARCHITECTURE and DECISIONS. It lives on every user's machine.
8. Do not rename linked headings: `grep -rn "FILE.md#" .` first (Go and `ci.yaml` cite them too). Never ROADMAP's "Not built yet in the OpenSandbox API" (runtime 501s) or DECISIONS.md headings.
9. Do not invent. If code and a doc disagree, the code wins: fix the doc in the same PR.

## Docs contract

Keep PRs small and fill in `.github/pull_request_template.md`. A PR that changes user-visible
behaviour updates these docs in the same PR, or says why none are needed.

| If the PR changes... | Update |
|---|---|
| A command, subcommand or flag | `internal/app/help.go`, `docs/CLI.md` (every command row that takes a shared flag) |
| A `sbx serve` flag, an `SBX_*` env var, a feature gate | `docs/CLI.md` (gates: "Gated features" and the `sbx features` row) |
| A `sandbox.json` field or its validation | [docs/SPEC.md](docs/SPEC.md); an `examples/` spec if it shows the feature |
| An error message, a failure mode, or a fix for one | [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md): symptom, cause, fix; a fixed bug's entry names the fixed version |
| A headline feature, added or removed | [docs/GUIDES.md](docs/GUIDES.md) if it is a new shape of use; README only if it changes the pitch or the usage block |
| A built-in template (`examples/<name>/`) | `examples/README.md` table; the README "Templates:" line; `docs/SPEC.md` or `docs/CLI.md`; `scripts/pin-templates.sh`; the `for t in` loop in `scripts/usecases-e2e.sh` |
| An MCP tool (`internal/mcp/tools.go`), or how agents drive sbx | [docs/GUIDES.md](docs/GUIDES.md#ai-agents) tool list; every page that states the tool count |
| An OpenSandbox endpoint built or refused | ROADMAP's "not built" list, `test/osb/expectations` |
| Where something runs or is verified | [platform status](docs/ARCHITECTURE.md#platform-status) |
| A performance number or a comparison | [docs/BENCHMARKS.md](docs/BENCHMARKS.md) (script, machine, version); [docs/COMPARISON.md](docs/COMPARISON.md) (dated vendor link) |
| Wake, sleep, freeze, proxy, a package or addressing | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and a [DECISIONS.md](docs/DECISIONS.md) entry; label a now-stale figure in BENCHMARKS and README (do not delete it) |
| A roadmap item that shipped; anything a user would notice | remove it from [docs/ROADMAP.md](docs/ROADMAP.md); one line in [docs/release-notes/UNRELEASED.md](docs/release-notes/UNRELEASED.md) |

CI checks what a machine can: `internal/app/clidoc_test.go` (commands, serve flags, templates),
`internal/mcp/doccount_test.go` ("N tools"), `scripts/lint-docs-contract.sh` (`SBX_*` variables,
ROADMAP heading, dated Lessons, release index), `scripts/check-unreleased.sh` (UNRELEASED.md line).

## Doc map

One home per fact; other pages link to it. Numbers live in BENCHMARKS, flags and env vars in CLI.md,
fields in SPEC.md, platform caveats and terms in ARCHITECTURE. Grep for the home before adding a fact.

| Page | Type ([Diátaxis](https://diataxis.fr/)) | Home of |
|---|---|---|
| `README.md` | landing | two paragraphs, four proof bullets, install, one usage block, AI agents, [docs links](README.md#documentation). No tables, glossary or comparison. |
| `docs/QUICKSTART.md` | tutorial | a first success in ten minutes |
| `docs/GUIDES.md`, `examples/*/README.md` | how-to | every task, one section each, [AI agents](docs/GUIDES.md#ai-agents); one template each |
| `docs/CLI.md`, `docs/SPEC.md`, `docs/TROUBLESHOOTING.md` | reference | commands, flags, env vars; `sandbox.json`; symptom, cause, fix |
| `docs/ARCHITECTURE.md`, `docs/DECISIONS.md` | explanation | how it works, [platform status](docs/ARCHITECTURE.md#platform-status), [terms](docs/ARCHITECTURE.md#terms); why (dated; headings and old entries never changed) |
| `docs/BENCHMARKS.md`, `docs/COMPARISON.md`, `SECURITY.md` | explanation | every measured number; other tools, with dated vendor links; threat model and reporting |
| `docs/ROADMAP.md`, `docs/release-notes/`, `CONTRIBUTING.md` | project | what's next; what changed per tag; setup, test tiers, release |
| [`docs/design/*`](docs/design/README.md) | record | dated plans and spikes; never updated, never linked from release notes |

Removed pages: `docs/README.md` → README, `docs/STYLE.md` → this file, `USE-CASES.md` and `AI-AGENTS.md` → GUIDES.

## Writing docs

- One page, one type ([Google style guide](https://developers.google.com/style/highlights) for anything not here). Open with at most one short line, only if the title is not enough.
- State current behaviour once: no "used to" or "since v0.13". Fold a new feature into each page's opening, not a new section.
- Commands in fenced blocks, correct at the current release (`internal/app/`, `internal/daemon/serve.go`, `internal/spec/`).
- Links are relative in the repo, absolute and tag-pinned in release notes. A reference-style link needs its definition in the same file.
- Release notes: [TEMPLATE.md](docs/release-notes/TEMPLATE.md) holds the format and the length limit.

### Voice

- Second person, active, present tense. Lead with what the reader gets; lead with a code block when they want to do something.
- Sentences ≤ ~25 words, paragraphs ≤ 3 sentences, one-line bullets, terse table cells.
- Explain an outside term (MCP, microVM, egress...) once per page in a few words, or link [terms](docs/ARCHITECTURE.md#terms), where a new term gets a row.
- Internal names (`execd`, seal, slot, activator, jailer) stay off user pages unless the reader types or sees them.
- "Sandbox", never "box" or "env"; "preview feature" (also "gated feature"). No emoji. Code font for commands, flags, fields, files.
- A benefit claim carries a sourced number, a test or a link. Never trade a caveat for a stronger sentence. No meta-commentary about past mistakes.
- Sound human. No bold lead-ins or bold emphasis (a rare **Warning:** is fine), no tables for prose, no
  marketing headings ("Install", "Usage", "Limits" instead), no slogans ("X, not Y", "just works"), triplets
  or "Proof:" labels, no em-dash or semicolon chains, no "Note that", "Simply", "Seamlessly". Do not
  restate the heading or cite on every paragraph. The skill `review-docs` lists the checks.

### Numbers and sources

- A number we measured names its script, machine and version or date, in BENCHMARKS.md. Other pages link there.
- A vendor number is quoted, linked and dated ("checked 2026-09-27"). The reviewer opens the link.
- Never round in our favour, drop a caveat that changes the meaning, or infer a figure. Label a stale figure with its version.
- A/B rounds are interleaved. A delta inside the run-to-run spread is "not resolvable".

### Status vocabulary

Start every status cell with one of four labels, then the evidence. No softer words ("works", "supported").

| Label | Means |
|---|---|
| **verified in CI** | A CI job runs this path end to end on every change. Name the job. |
| **unit-tested** | Tests cover the logic with fakes; no real end-to-end run is gated. |
| **run by hand** | Someone ran the real path outside CI. Say where and at what version. |
| **not yet run end to end** | Built, maybe unit-tested; nobody has run the real path (say where). |

## Lessons (append, don't rewrite)

Append one dated line per hard-won lesson: `- YYYY-MM-DD · rule. Evidence (commit or file:line).`
Never edit or delete earlier lines. Past about 25, move the oldest to [CONTRIBUTING.md](CONTRIBUTING.md#lessons-archive).

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
- 2026-09-27 · Docs written by agents in committee read as AI-generated: bold lead-ins, tables for
  prose, slogans. One voice, plain bullets, a README of about 100 lines.
