---
name: release-notes
description: Draft sbx release notes for a new tag from docs/release-notes/UNRELEASED.md and the git log, in the format of docs/release-notes/TEMPLATE.md. Use when asked to "write the release notes", "prepare vX.Y.Z", "cut a release", "draft the changelog", or to fix or review a file in docs/release-notes/ in the sbx repo.
---

# Write sbx release notes

The release workflow publishes `docs/release-notes/vX.Y.Z.md` as the GitHub release body and
appends GitHub's generated commit list below it. The note is for someone deciding whether to
upgrade. Format: `docs/release-notes/TEMPLATE.md`. Style: `AGENTS.md#writing-docs`.

## 1. Collect

```sh
PREV=$(git describe --tags --abbrev=0)          # last tag (fetch tags first if the clone is shallow)
git log --no-merges --format='%h %s' "$PREV"..HEAD
cat docs/release-notes/UNRELEASED.md
```

- UNRELEASED.md is the primary source. Cross-check it against the log: every user-visible commit
  should have a line; add any that are missing, drop internal-only ones (CI, tests, refactors).
- For each item, confirm the behaviour in the code (command, flag, default). Do not trust a
  commit subject alone.

## 2. Write `docs/release-notes/vX.Y.Z.md`

- Copy TEMPLATE.md. Keep its section names and order. Map UNRELEASED's sections onto it:
  Breaking + Changed → "Before you upgrade" (Breaking / Behaviour change); Added → Highlights
  (or a short "Also new" list); Fixed → Fixes.
- Title names the feature the way the user types it (`sbx connect`, `--provider firecracker`).
  No puns, no essay titles.
- Breaking and behaviour changes first, labelled, each with what the user must do.
- Highlights: benefit sentence, one command or snippet, link to the doc. One number at most,
  with its script; it must already be in BENCHMARKS.md.
- Fixes are user-visible symptoms, one line each.
- Known limitations: a label from `AGENTS.md#status-vocabulary` (**not yet run end to end** on X).
- Length: TEMPLATE's limit. Rationale goes to DECISIONS.md, not here.
- Explain every outside term at first use (wording: `README.md#glossary`): the note is read alone.

## 3. Links

- Absolute and pinned to the tag: `https://github.com/aryanmehrotra/sbx/blob/vX.Y.Z/docs/...`.
- Images: `https://raw.githubusercontent.com/aryanmehrotra/sbx/vX.Y.Z/...`, committed before tagging.
- No relative links (they 404 in the release body). No links into `docs/design/`.

## 4. Finish

- Reset UNRELEASED.md to its header and empty Breaking / Added / Changed / Fixed sections.
- Bump the version stamps: the `docs/release-notes/README.md` index row, the supported version
  in `SECURITY.md`, README's "Platform status" line, ROADMAP's "As of" line and "Shipped recently".
- `SKIP_LINK_CHECK=1 bash scripts/lint-docs.sh` passes (it checks pinned URLs against git), and
  `bash scripts/lint-docs-contract.sh` (it fails on a note missing from the index).
- Never rewrite a published note. To correct one, add a dated "Update:" line at the top.
- Never promise a version for unbuilt work; say "not built yet, see ROADMAP".
- Then follow the tag steps in `CONTRIBUTING.md` ("Cutting a release").
