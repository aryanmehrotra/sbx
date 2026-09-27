#!/usr/bin/env bash
# A pull request that changes shipped code says what a user will notice, or says it is internal.
#
#   BASE=<base sha> PR_BODY="<the PR description>" scripts/check-unreleased.sh
#
# Run by ci.yaml on pull_request. Shipped code is main.go and internal/, minus _test.go. If any
# of it changed, the PR must either touch docs/release-notes/UNRELEASED.md or tick
# "- [x] No: internal only" in the PR template. Without this, the release-notes file is filled
# in from memory at tag time, and the fixes nobody remembered are the ones users hit.
#
# The same diff also *warns* (never fails) when a file the docs contract ties to a page changed
# without that page: a command without docs/CLI.md, a spec field without docs/SPEC.md, an MCP
# tool without docs/GUIDES.md. Warnings, because plenty of edits to those files are
# internal; a reviewer decides.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

base="${BASE:?set BASE to the pull request base sha}"
body="${PR_BODY:-}"

if ! changed=$(git diff --name-only "$base"...HEAD); then
  echo "::error::cannot diff against $base (is the checkout shallow? ci.yaml sets fetch-depth: 0)"
  exit 1
fi

# Shipped code only: a change to a _test.go file is never something a user notices.
code=$(grep -E '^(main\.go|internal/.*\.go)$' <<<"$changed" | grep -v '_test\.go$' || true)

touched() { grep -qE "$1" <<<"$changed"; }

warn_unless() { # $1 shipped-code pattern, $2 doc file, $3 what
  if grep -qE "$1" <<<"$code" && ! touched "^$2\$"; then
    echo "::warning::$3 changed but $2 did not. If users see the change, update it (AGENTS.md#docs-contract)."
  fi
}

warn_unless '^internal/app/(app|help)\.go$' docs/CLI.md "a command or its help (internal/app)"
warn_unless '^internal/daemon/serve\.go$' docs/CLI.md "sbx serve's flags (internal/daemon/serve.go)"
warn_unless '^internal/spec/' docs/SPEC.md "the sandbox.json parser (internal/spec)"
warn_unless '^internal/mcp/tools\.go$' docs/GUIDES.md "the MCP tools (internal/mcp/tools.go)"

[ -n "$code" ] || { echo "no shipped Go code changed; UNRELEASED.md not required"; exit 0; }

if touched '^docs/release-notes/UNRELEASED\.md$'; then
  echo "shipped code changed and docs/release-notes/UNRELEASED.md has a line: ok"
  exit 0
fi

if grep -qiE '^[[:space:]]*- \[x\] No: internal only' <<<"$body"; then
  echo "shipped code changed; the PR says it is internal only: ok"
  exit 0
fi

echo "shipped code changed:"
sed 's/^/  /' <<<"$code"
echo "::error::add one line to docs/release-notes/UNRELEASED.md, or tick '- [x] No: internal only' in the PR description"
exit 1
