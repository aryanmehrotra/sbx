#!/usr/bin/env bash
# Every reference-style link in the docs must resolve.
#
#   scripts/lint-docs.sh
#
# `[vendor][neon]` with no `[neon]:` definition does not fail anything — it renders as the
# literal text "[vendor][neon]" and looks, at a glance, like a citation. Four of them sat in
# COMPARISON.md's vendor table, which is the one place in this repo where a citation that is
# not a link is worse than no citation at all: the document's whole claim is that every
# figure is either measured here or quoted with a link.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0

for f in "$ROOT"/docs/*.md "$ROOT"/README.md "$ROOT"/console/README.md; do
  [ -f "$f" ] || continue

  for u in $(grep -o '\]\[[A-Za-z0-9_-]*\]' "$f" 2>/dev/null | sed 's/^\]\[//; s/\]$//' | sort -u); do
    if ! grep -q "^\[$u\]:" "$f"; then
      printf '  ✗ %s cites [%s], which is never defined\n' "${f#"$ROOT"/}" "$u"
      fail=1
    fi
  done
done

# A relative link to a file that is not there is the same class of problem.
for f in "$ROOT"/docs/*.md "$ROOT"/README.md; do
  [ -f "$f" ] || continue

  for target in $(grep -o '](\.\?\.\?/\?[A-Za-z0-9._/-]*\.md)' "$f" 2>/dev/null | sed 's/^](//; s/)$//'); do
    if [ ! -f "$(dirname "$f")/$target" ]; then
      printf '  ✗ %s links to %s, which does not exist\n' "${f#"$ROOT"/}" "$target"
      fail=1
    fi
  done
done

[ "$fail" = 0 ] && echo "  ✓ every documentation link resolves"
exit "$fail"
