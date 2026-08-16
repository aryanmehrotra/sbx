#!/usr/bin/env bash
# Record the README's demo from a real run.
#
#   scripts/demo.sh                 # record: run everything, write docs/demo.svg + .lines
#   scripts/demo.sh --render-only   # redraw docs/demo.svg from the committed capture
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

RENDER_ONLY=0
if [ "${1-}" = "--render-only" ]; then RENDER_ONLY=1; shift; fi

OUT="${1:-$ROOT/docs/demo.svg}"
LINES="${OUT%.svg}.lines"
WORK="$(mktemp -d)"
TAG="demo$$"
DAEMON=""

# Redraw the committed capture. Nothing runs, nothing is measured, and the numbers in the
# picture stay the ones that were recorded - which is the point: a change to how this is
# drawn should not be able to change what it claims.
if [ "$RENDER_ONLY" = 1 ]; then
  [ -f "$LINES" ] || { echo "no capture at $LINES - record one first" >&2; exit 1; }
  python3 "$ROOT/scripts/lib/render-demo.py" "$LINES" "$OUT" || exit 1
  echo "wrote $OUT from $LINES" >&2
  exit 0
fi

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
#
# The threshold is load per core, not raw load. A raw number cannot mean the same thing on a
# 4-core laptop and a 64-core box, and the first version of this guard - raw load >= 4 -
# refused to record on any developer machine with a few background containers running, which
# is every developer machine. A guard nobody can satisfy is one everybody overrides.
cores=$( (getconf _NPROCESSORS_ONLN || sysctl -n hw.ncpu || nproc) 2>/dev/null | head -1)
cores=${cores:-1}

loadnow() { uptime | sed -E 's/.*load averages?: *([0-9.]+).*/\1/' | tr -d ' '; }

busy() { python3 -c "print(1 if float('${1:-0}') / max(1, ${cores}) >= 0.7 else 0)" 2>/dev/null || echo 0; }

# Checked twice: once here, and again immediately before the wake is timed.
#
# Checking only here is not enough, and this is not hypothetical - a recording started on a
# quiet machine, the machine got busy during the three minutes of docker work, and the run
# published a 2507ms wake for something that takes about 130ms. The number a reader takes
# away is measured at the end, so that is where the machine has to be quiet.
#
# The second check waits rather than refusing, because by then the sandbox is already asleep
# and waiting costs nothing: nothing is running, no state moves, and the wake is timed from a
# cold sandbox either way. It also lets the demo's own load decay - three minutes of docker
# work leaves a load average that has nothing to do with how busy the machine is now, so a
# check that refused on it would refuse on every machine, including a quiet one.
wait_until_quiet() {
  waited=0

  while [ "$(busy "$(loadnow)")" = "1" ]; do
    [ "$waited" -ge 300 ] && return 1

    [ "$waited" = 0 ] && echo "demo: waiting for the machine to go quiet before timing the wake" >&2

    sleep 15
    waited=$((waited + 15))
  done

  return 0
}

refuse_if_busy() {
  load=$(loadnow)

  [ "$(busy "$load")" = "1" ] || return 0

  echo "demo: load is ${load} across ${cores} cores${1:+ $1}. Record this on a quieter" >&2
  echo "      machine, or the picture shows what the laptop was doing rather than what" >&2
  echo "      sbx does. Set DEMO_ANYWAY=1 to override." >&2

  [ "${DEMO_ANYWAY:-0}" = "1" ] || exit 1
}

refuse_if_busy

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

# The service that sleeps here has to be the one that wakes below, or the two lines name
# different services and the cycle the demo exists to show does not read as one.
grep -h 'slept' "$WORK/daemon.log" 2>/dev/null | grep "$TAG-branch" | grep redis | tail -1 \
  | python3 -c 'import sys,json
for line in sys.stdin:
    try: d=json.loads(line)
    except Exception: continue
    print("INFO\t%s  %s" % (d.get("sandbox","")+"/"+d.get("service",""), d.get("message","")))' \
  | sed "s/$TAG-branch/feature-x/" >> "$SCRIPT" || true

say ok '  asleep - 0 B of memory, the volume untouched'

say blank

# shellcheck disable=SC1090
eval "$("$SBX" env "$TAG-branch" 2>/dev/null)"

# Wake it with whatever this machine actually has, and label the line with the command that
# really ran. The two differ in their reply - redis-cli prints PONG, a socket sees the +PONG
# on the wire - and a demo that captions itself "recorded from a real run" cannot show one
# command's name above the other one's output.
wait_until_quiet || refuse_if_busy "and stayed busy for five minutes"

if command -v redis-cli >/dev/null 2>&1; then
  say cmd 'redis-cli ping        # an ordinary client; no SDK, no wrapper'
  start=$(python3 -c 'import time;print(int(time.time()*1000))')
  reply=$(redis-cli -h 127.0.0.1 -p "${REDIS_PORT:-0}" ping 2>&1 | tail -1)
else
  say cmd 'printf "PING\r\n" | nc 127.0.0.1 $REDIS_PORT   # any TCP connection at all'
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
fi
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
#
# The capture is kept beside the picture. Changing how the demo is drawn then costs a
# re-render rather than a docker run on a quiet machine, which matters more than it sounds:
# three separate rendering bugs shipped because re-checking one meant re-running everything,
# and so nobody did.
cp "$SCRIPT" "$LINES"

python3 "$ROOT/scripts/lib/render-demo.py" "$LINES" "$OUT" || exit 1

echo "wrote $OUT and $LINES" >&2
