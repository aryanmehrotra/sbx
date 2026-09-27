#!/usr/bin/env bash
# The parts of the docs contract (AGENTS.md) that a grep can check.
#
#   scripts/lint-docs-contract.sh
#
# lint-docs.sh checks that links resolve; this checks that the pages which must list things
# still list them. Each rule below exists because the contract asked for it in prose and prose
# alone did not hold. The command and `sbx serve` flag rows are checked by a Go test instead
# (internal/app/clidoc_test.go, with the templates), and the MCP tool count by
# internal/mcp/doccount_test.go, because those lists live in Go.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1
fail=0

# 1. Every SBX_* env var the code reads is in docs/CLI.md.
#
# Any "SBX_..." string literal in non-test Go, not just Getenv calls: some names are constants
# (fc.FirewallEnv) or go through envOr, and a grep for Getenv alone misses those. A literal that
# ends in "_" is a prefix the code appends to (SBX_CONNECT_TOKEN_<label>), not a variable, so the
# pattern requires the last character to be a letter or digit. Test-only switches
# (SBX_FC_E2E...) live in _test.go and are for CONTRIBUTING, not the user reference.
#
# The literal match is broad on purpose, so a few strings that only look like variables are
# listed here with where they come from. SBX_JSON is the heredoc delimiter `sbx install` writes
# into its daemon.json step (internal/cli/install_runtime.go), not something read from the
# environment. Add to this list only a name you have checked is never passed to Getenv.
not_env='^(SBX_JSON)$'
for v in $(grep -rhoE '"SBX_[A-Z0-9_]*[A-Z0-9]"' --include='*.go' --exclude='*_test.go' internal main.go \
             | tr -d '"' | sort -u | grep -Ev "$not_env"); do
  if ! grep -q "\`$v\`" docs/CLI.md; then
    printf '  ✗ docs/CLI.md does not document %s, which the code reads\n' "$v"
    fail=1
  fi
done

# 2. No "ROADMAP §N" outside the dated design records.
#
# ROADMAP.md lost its numbered sections at v0.14; a "§1" in a live page or a Go comment now
# points at nothing. The records in docs/design/ keep theirs (docs/design/README.md explains
# them). What must not move is the heading runtime errors and ci.yaml cite: see rule 8 in AGENTS.md.
if hits=$(grep -rn 'ROADMAP §' --include='*.md' --include='*.go' --include='*.yaml' --include='*.sh' \
            --exclude-dir=.git --exclude-dir=design --exclude='lint-docs-contract.sh' . ); then
  printf '%s\n' "$hits" | sed 's/^/  ✗ cites a ROADMAP section number that no longer exists: /'
  fail=1
fi

# 3. The heading the runtime points at still exists.
#
# internal/osb and internal/execd answer an unbuilt endpoint with "not built yet: docs/ROADMAP.md",
# and COMPARISON links the anchor. Renaming it silently strands every one of those readers.
if ! grep -q '^#\+ Not built yet in the OpenSandbox API$' docs/ROADMAP.md; then
  echo '  ✗ docs/ROADMAP.md lost the heading "Not built yet in the OpenSandbox API" (runtime errors and ci.yaml point at it)'
  fail=1
fi

# 4. Every Lessons entry in AGENTS.md is dated and has the house shape.
#
# "- YYYY-MM-DD · rule. Evidence." The date is what makes the log an audit trail, and undated
# lines were how every lesson came to carry the day the file was written, not the day learned.
if bad=$(sed -n '/^## Lessons/,/^## [^L]/p' AGENTS.md | grep -E '^- ' | grep -vE '^- 20[0-9]{2}-[0-9]{2}-[0-9]{2} · '); then
  printf '%s\n' "$bad" | sed 's/^/  ✗ AGENTS.md Lessons line is not "- YYYY-MM-DD · rule": /'
  fail=1
fi

# 5. Every release note is in the index.
#
# docs/release-notes/README.md is the page people land on from the docs index; a tag cut
# without its row there is a release nobody browsing finds.
for f in docs/release-notes/v*.md; do
  v=$(basename "$f" .md)
  if ! grep -q "^| \[$v\]($v.md)" docs/release-notes/README.md; then
    printf '  ✗ docs/release-notes/README.md has no index row for %s\n' "$v"
    fail=1
  fi
done

[ "$fail" -eq 0 ] || { echo; echo "the docs contract (AGENTS.md#docs-contract) is not met"; exit 1; }
echo "docs contract: env vars, ROADMAP heading, lessons, release index all in order"
