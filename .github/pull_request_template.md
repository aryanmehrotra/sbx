## What and why

<!-- One or two sentences each. Link the issue if there is one. -->

## User-facing change?

- [ ] No: internal only (refactor, tests, CI, tooling)
- [ ] Yes, and **breaking**: say what a user must do  <!-- also goes under "Breaking" in UNRELEASED.md -->
- [ ] Yes, not breaking

## Docs (see the docs contract in AGENTS.md)

<!-- Tick what you updated. If nothing applies, say why in one line. -->

- [ ] `internal/app/help.go` + `docs/CLI.md`: command, flag, `sbx serve` flag or `SBX_*` env var
- [ ] `docs/SPEC.md`: `sandbox.json` field or validation
- [ ] `docs/TROUBLESHOOTING.md`: new or changed error message / failure mode
- [ ] `README.md`: feature list or platform-status table
- [ ] `docs/AI-AGENTS.md`: MCP tool, or how agents drive sbx
- [ ] `docs/ROADMAP.md`: an item shipped, or an OpenSandbox endpoint built
- [ ] `docs/ARCHITECTURE.md` / `docs/DECISIONS.md`: component, addressing, or a design decision
- [ ] `docs/BENCHMARKS.md` / `docs/COMPARISON.md`: a number or a vendor claim
- [ ] No docs needed, because:

## Release note

- [ ] One line added to `docs/release-notes/UNRELEASED.md` (or: not user-facing)

<!-- Paste the line here. Write it for a user: what they can now do, or the symptom that is fixed. -->

## Tests

- [ ] A test fails without this change and passes with it: <!-- name it -->
- [ ] Tiers run locally: <!-- e.g. go test -short ./..., ./scripts/e2e.sh 3 -->
- [ ] `bash scripts/lint-docs.sh` passes if docs changed

## Measurements (only if this claims to be faster or smaller)

<!-- Script, machine, version, rounds, median and spread. Interleaved and alternated?
     A delta inside the spread is "not resolvable", not a result. -->

## Lesson learned (optional)

<!-- Did something surprise you or break in a way the next person would repeat?
     Append one dated line to "Lessons" in AGENTS.md and paste it here. -->
