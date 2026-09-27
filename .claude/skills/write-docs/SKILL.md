---
name: write-docs
description: Write or update sbx documentation so it matches the code and the house style. Use when a change adds or alters a command, flag, SBX_* env var, sandbox.json field, error message, MCP tool, backend or platform status, or when asked to "document this", "update the docs", "write a how-to", "add to the README", or "fix this page" in the sbx repo.
---

# Write sbx docs

The rules live in `AGENTS.md` (docs contract, lessons) and `docs/STYLE.md` (page types, voice,
numbers, status vocabulary, glossary). Read both before writing. This skill is the procedure.

## 1. Find every page the change touches

- Look up the change in the docs contract table in `AGENTS.md`. Every matching row is a page to edit.
- `grep -rn "<command|flag|field|ENV_VAR>" README.md docs/ examples/ internal/app/help.go` to find
  existing mentions that are now wrong.
- If the change ships a roadmap item, it also comes out of `docs/ROADMAP.md`.
- Anything a user would notice also gets one line in `docs/release-notes/UNRELEASED.md`.

## 2. Get the facts from the code, not from other docs

- Commands and flags: `internal/app/app.go` (dispatch), `internal/app/help.go` (help text).
- `sbx serve` flags: `internal/daemon/serve.go`. Env vars: `grep -rn 'Getenv("SBX_' internal`.
- Spec fields: `internal/spec/`. MCP tools: `internal/mcp/tools.go` (one `Name:` per tool).
- Status: which CI job covers it (`.github/workflows/ci.yaml`). That decides the status label.
- If you can, run it: `go build -o sbx . && ./sbx <cmd> --help`, and paste real output.

## 3. Pick the page type, then write

- Tutorial, how-to, reference or explanation (`docs/STYLE.md`). Put the text on the page of the
  matching type; link from the others instead of copying.
- Open with what the reader gets. Apply `docs/STYLE.md#voice` (sentence and cell length, zero
  prior knowledge, internal names, persuade with proof).
- Explain every outside term at its first use on the page, using `docs/STYLE.md#outside-terms`.
- Fold a new feature into the page's opening and tables; do not append a section at the end.
- State current behaviour once. No project history in reference pages.
- Numbers and vendor claims: `docs/STYLE.md#numbers-and-sources`.
- Status: a label from `docs/STYLE.md#status-vocabulary`. Caveats go in the README
  platform-status table, not in hero text.
- Use the house words in `docs/STYLE.md#glossary` exactly.

## 4. Check before you finish

```sh
SKIP_LINK_CHECK=1 bash scripts/lint-docs.sh   # links, anchors, reference links, pinned URLs
bash scripts/lint-docs-contract.sh            # SBX_* vars in CLI.md, ROADMAP heading, lessons
go test ./internal/app/ -run CLIReference     # every command and serve flag in CLI.md
grep -rn "FILE.md#old-anchor" .               # if you renamed a heading
```

- Every code block copy-pastes and runs against the current code.
- Run the `review-docs` skill on your diff, or read its checklist and apply it yourself.
- If you learned something the next writer would get wrong, append one dated line to
  "Lessons" in `AGENTS.md`.
