---
name: review-docs
description: Critically review sbx documentation changes (a PR, a diff, or named pages) for accuracy against the code, audience, Diátaxis page type, readability, honesty of status and numbers, and links. Use when asked to "review the docs", "check this PR's docs", "is this page accurate", "critique the README", or before merging any PR that touches README.md, docs/, examples/ or release notes in the sbx repo.
---

# Review sbx docs

You are a critic, not a co-author. Find what is wrong, missing or misleading; do not rewrite the
page. Rules: `AGENTS.md` (docs contract, doc map, `#writing-docs`, lessons).

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
- For each user-visible code change, every matching row of the `AGENTS.md` docs contract was
  updated (shared flags, templates, wake behaviour, fixed-bug workarounds and the MCP tool count
  are the rows most often missed).
- `docs/release-notes/UNRELEASED.md` has a user-facing line; breaking changes are under Breaking.
- Shipped roadmap items were removed from `docs/ROADMAP.md`.

**3. Audience and page type**
- One home per fact (`AGENTS.md#doc-map`): a fact restated on a second page is a finding; link instead.
- The page is one Diátaxis type, and the new text fits it. Rationale or history in a reference
  page goes to DECISIONS.md.
- The page opens with what it is and who it is for. It leads with what the reader gets.
- A new feature is folded into the opening, not appended.

**4. Readability and zero prior knowledge** (`AGENTS.md#voice`)
- Sentence and table-cell length, one idea per paragraph, as `AGENTS.md#voice` sets them.
- Read it as someone who has never heard of OpenSandbox, MCP, Firecracker, microVMs, gVisor,
  Kata, CRIU or E2B: is every outside term explained in plain words at its first use *on this
  page* (wording: `README.md#glossary`)? A term explained only on another page is a finding.
- No internal name (`execd`, seal, slot, activator, jailer) on a user-facing page without an
  explanation next to it.
- Glossary terms used consistently; no meta-commentary about past mistakes in user docs.
- DECISIONS.md: check new entries only; old ones are records.

**5. Honesty and proof: numbers, claims and status**
- Numbers and vendor claims follow `AGENTS.md#numbers-and-sources`. Open each vendor link:
  does the page still say it?
- Persuade with proof: every benefit claim carries a number, a test or a link. An adjective
  alone ("fast", "secure") is a finding. So is a caveat dropped to make a claim stronger.
- Status cells start with a label from `AGENTS.md#status-vocabulary` and match what
  `ci.yaml` actually runs. Caveats are in the README platform-status table, not deleted.
- No version promised for unbuilt work.

**6. Links and anchors**
- `SKIP_LINK_CHECK=1 bash scripts/lint-docs.sh` and `bash scripts/lint-docs-contract.sh` pass.
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
