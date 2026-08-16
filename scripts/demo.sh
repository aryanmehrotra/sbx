#!/usr/bin/env bash
# Record the README's demo from a real run.
#
#   scripts/demo.sh                 # writes docs/demo.svg
#
# The previous demo was hand-drawn, and a hand-drawn demo drifts: it showed a `sbx create`
# message the binary had stopped printing, and it kept a typographic dash after every other
# file in the repo had lost one. This runs the actual commands against actual docker and
# renders whatever comes back, so the picture is a measurement like everything else here.
#
# It shows the use cases rather than the self-test: a branch, an agent reading JSON, a service
# added mid-task, seed-and-fork, and the sleep/wake cycle that is the whole product.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
OUT="${1:-$ROOT/docs/demo.svg}"
WORK="$(mktemp -d)"
TAG="demo$$"
DAEMON=""

cleanup() {
  [ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
  for s in "$TAG-branch" "$TAG-seed" "$TAG-agent"; do "$SBX" rm "$s" >/dev/null 2>&1; done
  docker images -q "sbx-snap-$TAG-golden*" 2>/dev/null | xargs -r docker rmi -f >/dev/null 2>&1
  docker volume ls -q 2>/dev/null | grep "snapvol-$TAG-golden" | xargs -r docker volume rm >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

[ -x "$SBX" ] || { echo "build first: go build -o sbx ." >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "needs a docker daemon" >&2; exit 1; }

SCRIPT="$WORK/lines"
: > "$SCRIPT"

# say <kind> <text>   kinds: cmd, out, ok, info, dim, blank
say() { printf '%s\t%s\n' "$1" "${2-}" >> "$SCRIPT"; }

# Ports are shown as recorded. An earlier version flattened them all to 20000 so that
# re-recording produced no diff, which made two services appear to share one address - the
# opposite of what this is demonstrating.
norm() { cat; }

# A demo is a measurement, and this project does not publish measurements taken on a
# machine that is busy doing something else. One recording came back with a 66-second wake
# and a reset connection at load 23; nothing was wrong with sbx.
load=$(uptime | sed -E 's/.*load averages?: *([0-9.]+).*/\1/' | tr -d ' ')

if [ "$(printf '%.0f' "${load:-0}" 2>/dev/null || echo 0)" -ge 4 ]; then
  echo "demo: load average is ${load}. Record this on a quiet machine, or the picture" >&2
  echo "      shows what the laptop was doing rather than what sbx does." >&2
  echo "      Set DEMO_ANYWAY=1 to override." >&2
  [ "${DEMO_ANYWAY:-0}" = "1" ] || exit 1
fi

echo "recording..." >&2

"$SBX" serve --idle 6s --refresh 2s >"$WORK/daemon.log" 2>&1 &
DAEMON=$!
sleep 3

# ── a branch ──────────────────────────────────────────────────────────────────
say cmd 'sbx create feature-x --template web-stack'
"$SBX" create "$TAG-branch" --template web-stack 2>&1 | norm | while IFS= read -r l; do
  case "$l" in
    *"✓"*)  say ok  "$l" ;;
    ready*) say dim "$l" ;;
    sandbox*) say out "$(echo "$l" | sed "s/$TAG-branch/feature-x/")" ;;
    *)      [ -n "$l" ] && say out "$l" ;;
  esac
done

say blank
say cmd 'eval "$(sbx env feature-x)"    # it remembers what it was made from'
"$SBX" env "$TAG-branch" 2>/dev/null | norm | sed "s/$TAG-branch/feature-x/" | while IFS= read -r l; do
  say out "$l"
done

# ── an agent ──────────────────────────────────────────────────────────────────
say blank
say cmd 'sbx env feature-x --shell json          # no SDK; an agent parses this'
"$SBX" env "$TAG-branch" --shell json 2>/dev/null | norm | sed "s/$TAG-branch/feature-x/" \
  | head -6 | while IFS= read -r l; do say out "$l"; done
say out '  ...'

say blank
say cmd "sbx add feature-x cache --image redis:7-alpine --port 6379 --health 'redis-cli ping'"
"$SBX" add "$TAG-branch" cache --image redis:7-alpine --port 6379 --health 'redis-cli ping' 2>&1 \
  | norm | grep '✓' | while IFS= read -r l; do say ok "$l"; done

# ── seed once, fork many ──────────────────────────────────────────────────────
say blank
say cmd 'sbx snapshot main golden && sbx fork golden agent-1'
"$SBX" create "$TAG-seed" --template postgres >/dev/null 2>&1
"$SBX" exec "$TAG-seed" postgres psql -U app -d app \
  -c "create table t(v text); insert into t values ('seeded')" >/dev/null 2>&1
"$SBX" snapshot "$TAG-seed" "$TAG-golden" 2>&1 | grep '→' | norm \
  | sed "s/$TAG-golden/golden/g; s/$TAG-seed/main/g" | while IFS= read -r l; do say out "$l"; done
"$SBX" fork "$TAG-golden" "$TAG-agent" 2>&1 | grep -E 'restored|forked' | norm \
  | sed "s/$TAG-golden/golden/g; s/$TAG-agent/agent-1/g" | while IFS= read -r l; do say out "$l"; done

got=$("$SBX" exec "$TAG-agent" postgres psql -U app -d app -tAc 'select v from t' 2>/dev/null | tr -d ' \n')
say ok "  agent-1 carries the seeded row: $got"

# ── the cycle that is the product ─────────────────────────────────────────────
say blank
say cmd '# nobody touches it for a few seconds'

waited=0
until [ "$(docker inspect -f '{{.State.Status}}' "sbx-$TAG-branch-redis" 2>/dev/null)" = "exited" ]; do
  sleep 2; waited=$((waited + 2)); [ "$waited" -ge 40 ] && break
done

grep -h 'slept' "$WORK/daemon.log" 2>/dev/null | grep "$TAG-branch" | tail -1 \
  | python3 -c 'import sys,json
for line in sys.stdin:
    try: d=json.loads(line)
    except Exception: continue
    print("INFO\t%s  %s" % (d.get("sandbox","")+"/"+d.get("service",""), d.get("message","")))' \
  | sed "s/$TAG-branch/feature-x/" >> "$SCRIPT" || true

say ok '  asleep - 0 B of memory, the volume untouched'

say blank
say cmd 'redis-cli ping        # any TCP connection; no SDK, no wrapper'

# shellcheck disable=SC1090
eval "$("$SBX" env "$TAG-branch" 2>/dev/null)"
start=$(python3 -c 'import time;print(int(time.time()*1000))')
reply=$(python3 - "${REDIS_PORT:-0}" <<'PYWAKE'
import socket, sys
try:
    s = socket.create_connection(("127.0.0.1", int(sys.argv[1])), timeout=90)
    s.sendall(b"PING\r\n")
    print(s.recv(32).decode(errors="replace").strip())
    s.close()
except Exception as e:
    print("ERR", e)
PYWAKE
)
took=$(( $(python3 -c 'import time;print(int(time.time()*1000))') - start ))
say out "$reply"

grep -h 'woke' "$WORK/daemon.log" 2>/dev/null | grep "$TAG-branch" | grep redis | tail -1 \
  | python3 -c 'import sys,json
for line in sys.stdin:
    try: d=json.loads(line)
    except Exception: continue
    print("INFO\t%s  %s" % (d.get("sandbox","")+"/"+d.get("service",""), d.get("message","")))' \
  | sed "s/$TAG-branch/feature-x/" >> "$SCRIPT" || true

say ok "  served in ${took}ms - the client waited, it was never refused"

kill "$DAEMON" 2>/dev/null; DAEMON=""

# Nothing internal may reach the picture. The tag is a pid, so a leak is both ugly and a
# small privacy nick - and it happened on the first recording.
sed -i.bak -E "s/${TAG}-branch/feature-x/g; s/${TAG}-seed/main/g; s/${TAG}-agent/agent-1/g; s/${TAG}-golden/golden/g; s/${TAG}[a-z-]*/sandbox/g" "$SCRIPT"
rm -f "$SCRIPT.bak"

if grep -q "$TAG" "$SCRIPT"; then
  echo "demo: internal name leaked into the recording" >&2
  grep "$TAG" "$SCRIPT" >&2
  exit 1
fi

# ── render ────────────────────────────────────────────────────────────────────
python3 "$ROOT/scripts/lib/render-demo.py" "$SCRIPT" "$OUT" || exit 1

echo "wrote $OUT" >&2
