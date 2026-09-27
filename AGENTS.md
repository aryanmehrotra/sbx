# AGENTS.md

Rules for anyone changing this repository: coding agents (Codex, Cursor, Copilot; Claude Code via
`CLAUDE.md`) and people. This file is the one home of the rules, the docs contract and the writing
style. [CONTRIBUTING.md](CONTRIBUTING.md) covers setup, tests and releases, and links here.
Using sbx *from* an AI agent is a different topic: [docs/GUIDES.md](docs/GUIDES.md#ai-agents).

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
| `internal/mcp/` | MCP server (stdlib JSON-RPC). Tools are defined in `tools.go`; `internal/app/mcp.go` only registers them. |
| `internal/spec/`, `internal/egress/` | `sandbox.json` parsing, validation, ports; the egress filter (policy, rules, microVM refusals). |
| `internal/tui`, `internal/ui`, `internal/tunnel`, `internal/ws*`, `internal/history`... | Dashboard, public links, WebSocket halves, audit history. Each package opens with a doc comment. |
| `scripts/` | e2e suites, benchmarks (`bench*.sh`, `compare.sh`, `connbench.sh`), linters, release helpers. |
| `test/osb/`, `console/` | OpenSandbox upstream conformance harness; optional log console. Each is its own Go module. |
| `deploy/`, `examples/` | Activator image and service files; built-in specs (embedded into the binary). |
| `docs/` | The pages in the [doc map](#doc-map); `docs/design/` = dated design records; `docs/release-notes/` = one file per tag. |

## Build and test

```sh
go build -o sbx . && ./sbx doctor     # what this machine can do
go test -short ./...                  # unit, no docker (fast; run always)
go test ./...                         # + docker-backed tests (~1 min)
go vet ./... && gofmt -l .            # CI fails on either
./sbx selftest                        # whole cycle end to end, ~9 s with images local
SKIP_LINK_CHECK=1 bash scripts/lint-docs.sh && bash scripts/lint-docs-contract.sh   # docs
bash scripts/platforms.sh             # all 8 GOOS/GOARCH build + vet (vet type-checks _test.go)
```

Heavier tiers (e2e, fork, recovery, conformance, microVM): [CONTRIBUTING.md](CONTRIBUTING.md#test-tiers). Run the one covering your change.

## Hard rules

1. **Zero Go dependencies.** The root `go.mod` has no `require`; CI enforces it. It is a product
   claim (`go install` is one step). Use the standard library.
2. **A test that fails without the change.** Break the code and watch it go red.
3. **Measurements** name the script, machine and version: see [Numbers and sources](#numbers-and-sources).
4. **Vendor claims** are quoted from the vendor, linked and dated. No inferred figures.
5. **Error messages say what to do next**: the command, the value, the one-liner that checks it.
6. **Comments explain why**, especially why the obvious thing was not done.
7. **Do not change addressing** (ports, slots, labels; it lives on every user's machine)
   without reading ARCHITECTURE and DECISIONS.
8. **Do not rename headings others link to**, especially ROADMAP's "Not built yet in the
   OpenSandbox API" (runtime 501s and `ci.yaml` point at it). Run `grep -rn "FILE.md#" .` first;
   Go comments and `ci.yaml` cite doc paths too. DECISIONS.md headings are never renamed.
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
| A headline feature, added or removed | README feature list (one line); [docs/GUIDES.md](docs/GUIDES.md) if it is a new shape of use |
| A built-in template (`examples/<name>/`) | `examples/README.md` table; README template row; `docs/SPEC.md` or `docs/CLI.md` (a test checks); `scripts/pin-templates.sh`; the `for t in` loop in `scripts/usecases-e2e.sh` |
| An MCP tool (`internal/mcp/tools.go`), or how agents should drive sbx | [docs/GUIDES.md](docs/GUIDES.md#ai-agents) (tool list); every page that states the tool count (a test checks) |
| An OpenSandbox endpoint built or refused | ROADMAP's "not built" list (runtime errors point there), `test/osb/expectations` |
| Where something runs or is verified | README [platform-status table](README.md#platform-status) |
| A performance number | [docs/BENCHMARKS.md](docs/BENCHMARKS.md), with script, machine, version |
| A comparison with another tool | [docs/COMPARISON.md](docs/COMPARISON.md), with a dated vendor link |
| Wake, sleep, freeze or proxy behaviour | ARCHITECTURE.md's section for it; label a published figure that is now stale in BENCHMARKS and README (do not delete it) |
| A package, component or addressing | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md); a new entry in [docs/DECISIONS.md](docs/DECISIONS.md) for the why |
| A roadmap item that shipped | [docs/ROADMAP.md](docs/ROADMAP.md): remove it (the release note records it) |
| Anything a user would notice | one line in [docs/release-notes/UNRELEASED.md](docs/release-notes/UNRELEASED.md) |

"No docs needed" is a valid answer; say why in the PR description.

CI checks what a machine can: every command and `sbx serve` flag is in `docs/CLI.md`, every
built-in template in SPEC or CLI (`internal/app/clidoc_test.go`), every "N tools" is the real MCP
tool count (`internal/mcp/doccount_test.go`); every `SBX_*` variable the code
reads is in `docs/CLI.md`, the ROADMAP heading exists, Lessons are dated, and every release note is
indexed (`scripts/lint-docs-contract.sh`); a PR that changes `main.go` or `internal/` adds an
UNRELEASED.md line or ticks "No: internal only" (`scripts/check-unreleased.sh`). The rest is on
the author and the reviewer.

## Doc map

**One home per fact.** Each fact lives on exactly one page; every other page links to it. Caveats
live in the README platform-status table; numbers in BENCHMARKS (README shows at most six
headline figures); flags and env vars in CLI.md; `sandbox.json` fields in SPEC.md; terms in the
README glossary; rules in this file. Before adding a fact, grep for its home.

| Page | Type ([Diátaxis](https://diataxis.fr/)) | Home of |
|---|---|---|
| `README.md` | landing | pitch, install, why, short comparison, features, headline numbers, [platform status](README.md#platform-status), [docs index](README.md#docs), [glossary](README.md#glossary) |
| `docs/QUICKSTART.md` | tutorial | a first success in ten minutes |
| `docs/GUIDES.md` | how-to | every task, one section each; [AI agents](docs/GUIDES.md#ai-agents) (paste block, MCP, OpenSandbox SDKs) |
| `examples/*/README.md` | how-to | one template each |
| `docs/CLI.md`, `docs/SPEC.md`, `docs/TROUBLESHOOTING.md` | reference | commands, flags, env vars; `sandbox.json`; symptom, cause, fix |
| `docs/ARCHITECTURE.md`, `docs/DECISIONS.md` | explanation | how it works; why (dated entries, headings frozen) |
| `docs/BENCHMARKS.md`, `docs/COMPARISON.md` | explanation | every measured number; other tools, with dated vendor links |
| `docs/ROADMAP.md`, `docs/release-notes/`, `CONTRIBUTING.md` | project | what's next; what changed per tag; setup, tests, release |
| `SECURITY.md` | explanation + project | threat model; reporting and supported versions |
| `docs/design/*` | record | dated plans and spikes; never updated, never linked from release notes |

Removed pages and where their content went (use these targets, never the old names):
`docs/README.md` → [README.md#docs](README.md#docs) · `docs/STYLE.md` → [#writing-docs](#writing-docs)
· `docs/USE-CASES.md`, `docs/AI-AGENTS.md` → [docs/GUIDES.md](docs/GUIDES.md) (`#ai-agents` for
agent material). Release notes `v*.md` are pinned to their tag and keep the old names.

## Writing docs

Anything not covered here: the [Google developer documentation style guide](https://developers.google.com/style/highlights).

### Page shape

- One page, one type (table above). If a page needs two, split it or link out.
- Open with one to three lines: what the page is and who it is for.
- Reference states current behaviour once: no "used to" or "since v0.13" (history → DECISIONS, release notes).
- Fold a new feature into each page's opening and tables; do not append a section.
- Stable, descriptive, sentence-case headings; short sections; commands in fenced blocks; tables
  for reference; nothing only in an image. Humans and agents read the same page.
- DECISIONS.md entries are records: old entries are not restyled; these rules apply to new ones.

### Voice

- Second person, active, present tense. Lead with what the reader gets, then how, then why.
- Sentences ≤ ~25 words; table cells ≤ ~20 (longer is prose). One idea per paragraph.
- **Assume zero prior knowledge.** Explain every outside term (OpenSandbox, MCP, Firecracker,
  microVM, gVisor, CRIU, egress, warm pool...) in one plain clause at first use *on each
  user-facing page*, in the wording of the [README glossary](README.md#glossary). A new term gets
  a glossary row in the PR that first uses it.
- **Internal names stay internal.** `execd`, seal, slot, activator, jailer never appear on a
  user-facing page without a plain explanation beside them. If the reader never types it or sees
  it in output, leave it out.
- **House words**: "sandbox", never "box" or "env"; "preview feature" (also "gated feature").
- **Persuade with proof.** A benefit claim carries a sourced number, a test or a link. "Fast" or
  "secure" alone is not a claim. Never trade an honest caveat for a stronger sentence.
- No meta-commentary about past mistakes in user docs (use DECISIONS or Lessons). No emoji;
  **Warning:** in bold. Match the file's dash style. Commands, flags, fields, env vars, files in code font.

### Code blocks

- Copy-pasteable and correct at the current release (commands: `internal/app/app.go`, `help.go`;
  serve flags: `internal/daemon/serve.go`; fields: `internal/spec/`). Comments say what the line
  gets you, with a number only if it is sourced.

### Numbers and sources

- A number we measured names its script, machine and version or date, in BENCHMARKS.md. Other
  pages link there.
- A vendor number is quoted, linked and dated ("checked 2026-09-27"). The reviewer opens the link;
  `lint-docs.sh` only checks that it resolves.
- Never round in our favour, never drop a caveat that changes the meaning, never infer a figure.
  Label a stale figure with the version it was measured on.
- A/B rounds are interleaved and alternated. A delta inside the run-to-run spread is "not
  resolvable", and is described that way.

### Status vocabulary

Start every status cell with one of four labels, then the evidence. No softer words ("verified",
"works", "supported"). Platform and backend caveats live only in the
[README platform-status table](README.md#platform-status); update it in the PR that changes CI coverage.

| Label | Means |
|---|---|
| **verified in CI** | A CI job runs this path end to end on every change. Name the job. |
| **unit-tested** | Tests cover the logic with fakes; no real end-to-end run is gated. |
| **run by hand** | Someone ran the real path outside CI. Say where and at what version. |
| **not yet run end to end** | Built, maybe unit-tested; nobody has run the real path (say where). |

### Links

- Relative links inside the repo; absolute links pinned to the tag in release notes.
- A reference-style link needs its `[name]: https://...` line in the same file. `lint-docs.sh`
  checks links, anchors and reference links, not the linkers of a heading you rename (rule 8).

### Release notes

[TEMPLATE.md](docs/release-notes/TEMPLATE.md) is the format and holds the one length limit (about
120 lines). Title names the feature as the user types it; breaking changes first. Between releases
each PR adds one line to [UNRELEASED.md](docs/release-notes/UNRELEASED.md).

## Pull requests

Fill in `.github/pull_request_template.md`: what and why, the docs you touched (or why none),
the UNRELEASED.md line, the test that fails without the change, any measurement. Keep PRs small;
split refactors from behaviour changes. Doc skills (`.claude/skills/write-docs`, `review-docs`,
`release-notes`) are procedures over these rules; agents without skills read the `SKILL.md` files.

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
