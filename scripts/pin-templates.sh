#!/usr/bin/env bash
# Re-resolve every template image to a digest, and stamp the date.
#
#   make templates-refresh          resolve and rewrite
#   scripts/pin-templates.sh --check   fail if anything is unpinned (CI)
#
# A template is the first thing anybody runs, and `zenika/alpine-chrome:latest` means the
# first thing anybody runs can break without a commit touching this repo. Pinning by digest
# makes a template reproducible: the sandbox someone creates today is the one CI tested.
#
# The tag is KEPT alongside the digest — `postgres:16-alpine@sha256:...` — because a bare
# digest tells a reader nothing about what they are running. Docker resolves by digest and
# ignores the tag, so the tag is documentation that cannot drift.
#
# The guard that matters: a digest is only pinned if it names a MANIFEST LIST covering
# linux/amd64 and linux/arm64. Pinning an arch-specific manifest resolved on this laptop
# would produce templates that pull here and fail in CI, and the failure would look like a
# broken template rather than a bad pin.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK=0
[ "${1:-}" = "--check" ] && CHECK=1

fail=0

# Every image mentioned by any template, tag stripped of any existing pin.
images() {
  grep -h '"image"' "$ROOT"/examples/*/sandbox.json \
    | sed 's/.*"image": *"\([^"]*\)".*/\1/' \
    | sed 's/@sha256:.*//' \
    | sort -u
}

# resolve <image:tag> -> sha256:... on stdout, or nothing and a message on stderr.
resolve() {
  local img="$1" out digest

  out=$(docker manifest inspect "$img" 2>&1) || {
    echo "  cannot inspect $img: $(echo "$out" | head -1)" >&2
    return 1
  }

  # A manifest list, and it must carry both architectures anyone runs this on.
  echo "$out" | python3 -c '
import json, sys
d = json.load(sys.stdin)
mt = d.get("mediaType", "")
if "list" not in mt and "index" not in mt:
    print("not a manifest list", file=sys.stderr); raise SystemExit(1)
have = {m["platform"]["os"] + "/" + m["platform"]["architecture"]
        for m in d.get("manifests", []) if "platform" in m}
missing = {"linux/amd64", "linux/arm64"} - have
if missing:
    print("missing platforms: " + ", ".join(sorted(missing)), file=sys.stderr); raise SystemExit(1)
' 2>/tmp/pin-why || {
    echo "  refusing to pin $img: $(cat /tmp/pin-why)" >&2
    return 1
  }

  # The list digest, taken the way docker itself records it. `docker manifest inspect -v`
  # reports the descriptor it resolved, which is the list digest for a multi-arch image.
  docker pull -q "$img" >/dev/null 2>&1 || { echo "  cannot pull $img" >&2; return 1; }

  digest=$(docker inspect --format '{{index .RepoDigests 0}}' "$img" 2>/dev/null)
  digest="${digest#*@}"

  case "$digest" in
    sha256:*) echo "$digest" ;;
    *) echo "  no digest for $img" >&2; return 1 ;;
  esac
}

if [ "$CHECK" = "1" ]; then
  echo "checking every template image is pinned…"

  while IFS= read -r line; do
    case "$line" in
      *@sha256:*) ;;
      *) echo "  ✗ unpinned: $line"; fail=1 ;;
    esac
  done < <(grep -h '"image"' "$ROOT"/examples/*/sandbox.json | sed 's/.*"image": *"\([^"]*\)".*/\1/' | sort -u)

  [ "$fail" = 0 ] && echo "  ✓ all template images are pinned by digest"
  exit "$fail"
fi

command -v docker >/dev/null 2>&1 || { echo "docker is needed to resolve digests" >&2; exit 1; }

echo "resolving template images…"

for img in $(images); do
  if ! digest=$(resolve "$img"); then
    fail=1
    continue
  fi

  printf '  %-42s %s\n' "$img" "${digest:0:19}…"

  # Rewrite every occurrence, pinned or not, to this image at this digest.
  python3 - "$img" "$digest" "$ROOT" <<'PY'
import glob, os, re, sys

img, digest, root = sys.argv[1], sys.argv[2], sys.argv[3]

for path in glob.glob(os.path.join(root, "examples", "*", "sandbox.json")):
    with open(path) as f:
        body = f.read()

    # The image, optionally already carrying a digest, as a whole JSON string value.
    new = re.sub(r'("image": ")' + re.escape(img) + r'(@sha256:[a-f0-9]+)?(")',
                 r'\g<1>' + img + "@" + digest + r'\g<3>', body)

    if new != body:
        with open(path, "w") as f:
            f.write(new)
PY
done

if [ "$fail" != 0 ]; then
  echo
  echo "some images could not be pinned — nothing was stamped, so the date still reflects the"
  echo "last complete refresh rather than this partial one."
  exit 1
fi

# The date is what makes a pin honest. A pinned template with no visible age is one nobody
# ever refreshes, because nobody can tell that it needs it.
date -u +'{ "refreshed": "%Y-%m-%d" }' > "$ROOT/examples/pinned.json"

echo
echo "stamped $(cat "$ROOT/examples/pinned.json")"
echo "review the diff, run scripts/usecases-e2e.sh templates, then commit."
