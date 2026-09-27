# Unreleased

Changes merged since the last release, collected one line per pull request. At release time
they are folded into `vX.Y.Z.md` from TEMPLATE.md, and this file is reset to empty sections.
This file is never a release body itself.

How to add a line:

- Write for a user, not a reviewer: what they can now do, or the symptom that is fixed.
- One line, about 20 words, starting with the command, flag or field in code font when there is one.
- Put it under exactly one section. A behaviour change that needs action from a user is Breaking.
- No links, or absolute ones only: relative links break once this text becomes a release body.
- Internal changes (tests, CI, refactors, contributor and style docs) do not get a line. A new
  user guide does.
- A pull request that changes `main.go` or `internal/` either adds a line here or ticks "No:
  internal only" in the PR template; CI checks it (`scripts/check-unreleased.sh`).

Shapes: "`sbx COMMAND --FLAG` now does WHAT THE USER GETS." or "SYMPTOM no longer happens when CONDITION."

At release time, Breaking and Changed become the note's "Before you upgrade", Added becomes
"Highlights", and Fixed becomes "Fixes".

## Breaking

## Added

- New guides: a ten-minute quickstart (`docs/QUICKSTART.md`), a full CLI reference (`docs/CLI.md`: every command, `sbx serve` flag and `SBX_*` variable), and one how-to page, `docs/GUIDES.md`.

## Changed

- The README, comparison, roadmap and user guides are rewritten against v0.14. The use-case and AI-agent guides merge into `docs/GUIDES.md`; the docs index and glossary move into the README.

## Fixed
