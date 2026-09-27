#!/usr/bin/env bash
# Assemble the landing page into _site/ for GitHub Pages (or a local look).
#
#   scripts/site-build.sh            # then open _site/index.html
#
# site/ holds the page itself. Its pictures are the ones the README uses, copied from docs/ at
# build time rather than duplicated in the repo, so the chart on the site cannot drift from the
# one in the README.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/_site"

rm -rf "$OUT"
mkdir -p "$OUT/assets"
cp -R "$ROOT/site/." "$OUT/"
for f in bench-light.svg bench-dark.svg how-it-works.svg social-preview.png; do
  cp "$ROOT/docs/$f" "$OUT/assets/$f"
done
echo "built $OUT"
