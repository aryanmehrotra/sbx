## What and why

<!-- One or two sentences each. Link the issue if there is one. -->

## User-facing change?

- [ ] No: internal only (refactor, tests, CI, tooling)
- [ ] Yes, and **breaking**: say what a user must do  <!-- also goes under "Breaking" in UNRELEASED.md -->
- [ ] Yes, not breaking

## Docs (see the docs contract in AGENTS.md)

<!-- Tick every row of the AGENTS.md docs contract that applies. If nothing applies, say why in one line. -->

- [ ] `internal/app/help.go` + `docs/CLI.md`: command, flag, `sbx serve` flag, `SBX_*` env var or feature gate
- [ ] A flag shared by several commands: every `docs/CLI.md` row that takes it
- [ ] `docs/SPEC.md`: `sandbox.json` field or validation
- [ ] `docs/TROUBLESHOOTING.md`: new or changed error message; a workaround this fix makes obsolete removed
- [ ] `README.md`: feature list or platform-status table
- [ ] `docs/GUIDES.md` / `examples/`: a new shape of use, or a spec that shows the feature
- [ ] A built-in template: `examples/README.md`, README template row, SPEC or CLI, `scripts/pin-templates.sh`, `scripts/usecases-e2e.sh` loop
- [ ] `docs/GUIDES.md#ai-agents`: MCP tool (and the tool count everywhere), or how agents drive sbx
- [ ] `docs/ROADMAP.md` / `test/osb/expectations`: an item shipped, or an OpenSandbox endpoint built or refused
- [ ] `docs/ARCHITECTURE.md` / `docs/DECISIONS.md`: wake, sleep, freeze or proxy behaviour; component, addressing, or a design decision
- [ ] `docs/BENCHMARKS.md` / `docs/COMPARISON.md`: a number (or a published one now stale), or a vendor claim
- [ ] No docs needed, because:

## Release note

- [ ] One line added to `docs/release-notes/UNRELEASED.md` (or "No: internal only" ticked above;
      CI fails a change to `main.go` or `internal/` with neither)

<!-- Paste the line here. Write it for a user: what they can now do, or the symptom that is fixed. -->

## Tests

- [ ] A test fails without this change and passes with it: <!-- name it -->
- [ ] Tiers run locally: <!-- e.g. go test -short ./..., ./scripts/e2e.sh 3 -->
- [ ] `bash scripts/lint-docs.sh` passes if docs changed

## Measurements (only if this claims to be faster or smaller)

<!-- Script, machine, version, rounds, median and spread (AGENTS.md#numbers-and-sources). -->

## Lesson learned (optional)

<!-- Did something surprise you or break in a way the next person would repeat?
     Append one dated line to "Lessons" in AGENTS.md and paste it here. -->
