#!/usr/bin/env bash
# Every reference-style link in the docs must resolve.
#
#   scripts/lint-docs.sh
#
# `[vendor][neon]` with no `[neon]:` definition does not fail anything - it renders as the
# literal text "[vendor][neon]" and looks, at a glance, like a citation. Four of them sat in
# COMPARISON.md's vendor table, which is the one place in this repo where a citation that is
# not a link is worse than no citation at all: the document's whole claim is that every
# figure is either measured here or quoted with a link.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0

for f in "$ROOT"/docs/*.md "$ROOT"/docs/release-notes/*.md "$ROOT"/README.md "$ROOT"/console/README.md; do
  [ -f "$f" ] || continue

  for u in $(grep -o '\]\[[A-Za-z0-9_-]*\]' "$f" 2>/dev/null | sed 's/^\]\[//; s/\]$//' | sort -u); do
    if ! grep -q "^\[$u\]:" "$f"; then
      printf '  ✗ %s cites [%s], which is never defined\n' "${f#"$ROOT"/}" "$u"
      fail=1
    fi
  done
done

# A relative link to a file that is not there is the same class of problem.
for f in "$ROOT"/docs/*.md "$ROOT"/docs/release-notes/*.md "$ROOT"/README.md; do
  [ -f "$f" ] || continue

  for target in $(grep -o '](\.\?\.\?/\?[A-Za-z0-9._/-]*\.md)' "$f" 2>/dev/null | sed 's/^](//; s/)$//'); do
    if [ ! -f "$(dirname "$f")/$target" ]; then
      printf '  ✗ %s links to %s, which does not exist\n' "${f#"$ROOT"/}" "$target"
      fail=1
    fi
  done
done

# A #fragment must match a heading in the file it points at.
#
# `](../README.md#use-it)` is not a broken link - the file is there - so the target check
# above passes it, and the reader lands at the top of the page wondering what they missed.
# One of those shipped in the first line of USE-CASES.md.
python3 - "$ROOT" <<'PYANCHOR' || fail=1
import os, re, sys, glob

root = sys.argv[1]
bad = 0

files = ["README.md", "CONTRIBUTING.md", "SECURITY.md", "console/README.md"]
files += [os.path.relpath(p, root) for p in glob.glob(os.path.join(root, "docs", "*.md"))]
files += [os.path.relpath(p, root) for p in glob.glob(os.path.join(root, "docs", "release-notes", "*.md"))]

def anchors(path):
    heads = re.findall(r"^#{1,6} (.+)$", open(path).read(), re.M)
    return {re.sub(r"[^a-z0-9 -]", "", h.lower()).strip().replace(" ", "-") for h in heads}

for rel in files:
    path = os.path.join(root, rel)
    if not os.path.exists(path):
        continue

    for target, frag in re.findall(r"\]\(([^)#]*)#([a-z0-9-]+)\)", open(path).read()):
        dest = path if not target else os.path.normpath(os.path.join(os.path.dirname(path), target))

        if not os.path.exists(dest):
            continue  # the target check above already reports this

        if frag not in anchors(dest):
            print("  ✗ %s links to %s#%s, which is not a heading there"
                  % (rel, target or "(itself)", frag))
            bad = 1

raise SystemExit(bad)
PYANCHOR

# And the external ones must actually be there.
#
# Six rounds of review produced five separate vendor-sourcing defects in these files -
# invented ranges, a percentile nobody published, a cold start quoted as a wake, an inverted
# default, and a citation that 404s. It is the only defect class here with no automated gate,
# and it is the one this project can least afford: an unsourced number does not just mislead
# about the vendor, it discredits every measured number sitting beside it.
#
# This catches the dead-link half. The other half - a live URL whose page no longer says what
# is attributed to it - a linter cannot check, and the corrections table in COMPARISON.md is
# where that is tracked by hand.
#
# Skipped without a network rather than failing: a lint that fails on a plane is a lint people
# learn to bypass.
if [ "${SKIP_LINK_CHECK:-0}" = "1" ]; then
  echo "  - external links not checked (SKIP_LINK_CHECK=1)"
elif ! curl -fsS -m 10 -o /dev/null https://example.com 2>/dev/null; then
  echo "  - external links not checked (no network)"
else
  urls=$(grep -ho 'https://[A-Za-z0-9._~:/?#@!$&*+,;=%-]*' \
           "$ROOT"/docs/*.md "$ROOT"/docs/release-notes/*.md "$ROOT"/README.md 2>/dev/null \
         | sed 's/[.,)]*$//' | grep -E '^https://[A-Za-z0-9.-]+\.[A-Za-z]{2,}' | sort -u)

  for u in $urls; do
    # This project's own URLs 404 until it is published, which says nothing about whether the
    # link is right. The point of this check is other people's pages.
    case "$u" in
      *aryanmehrotra/sbx*) continue ;;
    esac

    # HEAD first; some hosts refuse it, so fall back to a ranged GET before believing a 404.
    code=$(curl -s -o /dev/null -w '%{http_code}' -I -L -m 20 "$u" 2>/dev/null)

    case "$code" in
      2*|3*) continue ;;
    esac

    code=$(curl -s -o /dev/null -w '%{http_code}' -L -m 20 -r 0-0 "$u" 2>/dev/null)

    case "$code" in
      2*|3*) continue ;;
      000)   printf '  - %s could not be reached (network, not a bad link)\n' "$u" ;;
      *)     printf '  ✗ %s returns %s\n' "$u" "$code"; fail=1 ;;
    esac
  done

  [ "$fail" = 0 ] && echo "  ✓ every external link resolves"
fi

# ── our own links point at a file that is really in that ref ──────────────────
#
# The check above deliberately skips this project's own URLs: they 404 until the thing is
# published, which says nothing about whether the link is right. Harmless until release notes
# existed - those are rendered outside the repository, so every picture and every doc link in
# them is an absolute URL of ours pinned to a ref, and the exemption meant none of them were
# checked at all. A release's hero image is exactly the link nobody notices is broken.
#
# So they are checked here, against git rather than the network: it is offline, and a tag that
# has not been pushed yet is not evidence of a bad link.
python3 - "$ROOT" <<'PY' || fail=1
import os, re, subprocess, sys, glob

root = sys.argv[1]
bad = 0

files = ["README.md", "CONTRIBUTING.md", "SECURITY.md"]
files += [os.path.relpath(p, root) for p in glob.glob(os.path.join(root, "docs", "*.md"))]
files += [os.path.relpath(p, root) for p in glob.glob(os.path.join(root, "docs", "release-notes", "*.md"))]

# Only the forms that name a ref and a path. Everything else of ours - releases, actions,
# compare - has no file behind it to check.
pats = [
    re.compile(r"https://raw\.githubusercontent\.com/aryanmehrotra/sbx/([^/\s]+)/([^)\s\"'>]+)"),
    re.compile(r"https://github\.com/aryanmehrotra/sbx/(?:blob|raw)/([^/\s]+)/([^)\s\"'>]+)"),
]

def known(ref):
    return subprocess.run(["git", "-C", root, "rev-parse", "--verify", "--quiet", ref + "^{commit}"],
                          capture_output=True).returncode == 0

for f in files:
    path = os.path.join(root, f)
    if not os.path.exists(path):
        continue

    for pat in pats:
        for ref, target in pat.findall(open(path, encoding="utf-8").read()):
            target = target.rstrip(".,)")

            if known(ref):
                ok = subprocess.run(["git", "-C", root, "cat-file", "-e", "%s:%s" % (ref, target)],
                                    capture_output=True).returncode == 0
                where = ref
            else:
                # A tag that does not exist yet: it will be cut from this commit, so what the
                # working tree has now is what that tag will contain.
                ok = os.path.exists(os.path.join(root, target))
                where = "the working tree (%s is not a ref here yet)" % ref

            if not ok:
                print("  ✗ %s: %s is not in %s" % (f, target, where))
                bad = 1

if not bad:
    print("  ✓ every link of ours names a file that is in the ref it pins")

sys.exit(bad)
PY

[ "$fail" = 0 ] && echo "  ✓ every documentation link resolves"

# ── the demo must be readable without animation ───────────────────────────────
#
# docs/demo.svg is the first thing anyone sees. Three versions of it shipped with the lines
# revealed one at a time by CSS, and every one of them rendered a blank terminal in a static
# snapshot - the social preview card, PDF export, editor previews, any screenshot - because
# the first frame of a reveal is by definition empty. Each time it looked perfect in a
# browser and the bug was found by accident. This is the check that finds it in a second.
if [ -f docs/demo.svg ]; then
  echo
  echo "the demo renders in a static snapshot"

  # The only animation allowed is the cursor blink, which is opaque at t=0. Anything else
  # animating opacity means some line is invisible in the first frame.
  if python3 - docs/demo.svg <<'PY'
import re, sys

css = "".join(re.findall(r"<style>(.*?)</style>", open(sys.argv[1]).read(), re.S))

# Drop the cursor blink and its keyframes; it is transparent for half a cycle by design and
# opaque in the frame a snapshot captures.
css = re.sub(r"\.cur\{[^}]*\}", "", css)
css = re.sub(r"@keyframes\s+blink\s*\{(?:[^{}]*\{[^{}]*\})*[^{}]*\}", "", css)

sys.exit(1 if re.search(r"opacity|animation", css) else 0)
PY
  then
    echo "  ✓ nothing but the cursor animates, so the first frame is the whole demo"
  else
    echo "  ✗ something other than the cursor animates or sets opacity - the demo will be"
    echo "    blank or partial in any static snapshot. Draw the lines, do not reveal them."
    fail=1
  fi

  texts=$(grep -c '<text' docs/demo.svg)

  if [ "$texts" -ge 20 ]; then
    echo "  ✓ $texts lines of real captured output"
  else
    echo "  ✗ only $texts lines - the recording was cut short"
    fail=1
  fi
fi

exit "$fail"
