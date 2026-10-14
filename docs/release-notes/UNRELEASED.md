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

- `egress: "deny"` is refused on Docker Desktop and colima, where the engine's VM routes the bridge out whatever masquerade says. Use `egress_policy` or `egress_allow`, which are enforced there.

## Added

- `credentialProxy` and the OpenSandbox Credential Vault API. An agent can hold a placeholder, or nothing, and the filter adds the real credential on the way out. No feature switch: the field on the create is the opt-in.
- `SBX_CREDENTIAL_VAULT_CA` names the authority a `credentialProxy` sandbox is given at `/etc/sbx/credential-vault-ca.pem`, for an entrypoint that adds it to the image's own trust store.
- `credentialProxy` on a host that cannot hold a hosted egress filter is refused with a 400 before any sandbox is made.
- `sbx doctor` reports `hosted egress filter`: whether `sbx serve` holds a sandbox's filter itself here, which is what `credentialProxy` needs.

## Changed

## Fixed

- `egress_allow`, `egress_policy` and `"egress": "allow"` work again on Docker Desktop and colima, where docker 28 refused the filter's fixed address.
- A create that fails now takes back the egress filter and bridge it made; `sbx rm` reclaims the ones left behind a sandbox that has no services, and `sbx gc` lists the ones a killed create left.
