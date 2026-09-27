---
name: review-docs
description: Critically review sbx documentation changes (a PR, a diff, or named pages) for accuracy against the code, audience, Diátaxis page type, readability, honesty of status and numbers, and links. Use when asked to "review the docs", "check this PR's docs", "is this page accurate", "critique the README", or before merging any PR that touches README.md, docs/, examples/ or release notes in the sbx repo.
---

# Review sbx docs

You are a critic, not a co-author. Find what is wrong, missing or misleading; do not rewrite the
page. Rules: `AGENTS.md` (docs contract, lessons) and `docs/STYLE.md`.

## Inputs

- The diff (`git diff main...HEAD -- README.md docs/ examples/ CONTRIBUTING.md SECURITY.md`) or
  the named pages. Also the code diff: a code change with no doc change is itself a finding.

## Checklist

**1. Accuracy against the code** (highest weight)
- Every command, flag, env var and spec field exists and is spelled as in `internal/app/app.go`,
  `internal/app/help.go`, `internal/daemon/serve.go`, `internal/spec/`.
- Every code block would run as written. Defaults and limits match the code.
- If a doc and the code disagree, the code wins: report the doc line and the code line.

**2. Docs contract coverage**
- For each user-visible code change, the pages in the `AGENTS.md` table were updated.
- `docs/release-notes/UNRELEASED.md` has a user-facing line; breaking changes are under Breaking.
- Shipped roadmap items were removed from `docs/ROADMAP.md`.

**3. Audience and page type**
- The page is one Diátaxis type, and the new text fits it. Rationale or history in a reference
  page goes to DECISIONS.md.
- The page opens with what it is and who it is for. It leads with what the reader gets.
- A new feature is folded into the opening, not appended.

**4. Readability**
- Sentences ≤ ~25 words, table cells ≤ ~20 words, one idea per paragraph.
- Jargon defined on first use; glossary terms used consistently.
- No meta-commentary about the project's past mistakes in user docs.

**5. Honesty: numbers and status**
- Every measured number names its script and links BENCHMARKS.md; machine and version are there.
- Every vendor claim is quoted, linked and dated. Open the link: does the page still say it?
- Status labels are exactly **verified in CI** / **unit-tested** / **not yet run end to end**,
  and match what `ci.yaml` actually runs. Caveats are in the README platform-status table, not
  deleted.
- No version promised for unbuilt work.

**6. Links and anchors**
- `SKIP_LINK_CHECK=1 bash scripts/lint-docs.sh` passes.
- A renamed heading: every `FILE.md#anchor` linker updated (`grep -rn`), including Go comments.
- Release notes: absolute links pinned to the tag, no relative links, no links into `docs/design/`.

## Output format

```
## Docs review: <PR or pages>
Verdict: ready | ready after fixes | not ready

### Must fix
1. <file>:<line> - <problem>. Evidence: <code path:line or quote>. Fix: <one line>.

### Should fix
1. ...

### Nits
1. ...

### Missing docs (contract)
- <code change> -> <page that should have changed>
```

Order by severity. Cite a file and line for every finding. Say "none" for an empty section.
If a finding is a recurring class of mistake, suggest a Lessons line for `AGENTS.md`.
