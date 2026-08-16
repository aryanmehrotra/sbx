#!/usr/bin/env bash
# Can the daemon be killed and restarted without anyone noticing?
#
#   scripts/recovery.sh
#
# Two commit messages already claim it: "the daemon owns no state - it rediscovers every
# sandbox from container labels at startup, and deliberately sleeps nothing on the way out."
# Nothing had checked either half, and both are the sort of claim that is true right up
# until someone adds a cache.
#
# So this kills it in the two states that matter - while a sandbox is awake, and while one
# is asleep - and asserts what a user would notice:
#
#   1. killing the daemon does not stop a running sandbox
#   2. a restarted daemon re-adopts sandboxes it never created
#   3. a sandbox that was asleep across the restart still wakes
#   4. the data is still there
#
# SIGKILL, not SIGTERM: a clean shutdown path proves nothing about a laptop that slept or an
# OOM kill, which is how this will actually die.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
NAME="recovery-$$"
SPEC_DIR="$(mktemp -d)"

[ -x "$SBX" ] || { echo "recovery: build first: go build -o sbx ." >&2; exit 1; }

cat > "$SPEC_DIR/sandbox.json" <<'EOF'
{
  "version": 1,
  "services": {
    "redis": { "image": "redis:7-alpine", "ports": [6379], "health": "redis-cli ping" }
  },
  "exports": { "REDIS_PORT": "redis:6379" }
}
EOF

pass=0; fail=0
ok()  { echo "  ✓ $1"; pass=$((pass + 1)); }
bad() { echo "  ✗ $1"; fail=$((fail + 1)); }

cleanup() {
  [ -n "${DAEMON:-}" ] && kill -9 "$DAEMON" 2>/dev/null
  "$SBX" rm "$NAME" >/dev/null 2>&1
  rm -rf "$SPEC_DIR"
}
trap cleanup EXIT

start_daemon() {
  "$SBX" serve --idle 5s --refresh 2s >/dev/null 2>&1 &
  DAEMON=$!
  sleep 3
}

container="sbx-${NAME}-redis"
running() { [ "$(docker inspect -f '{{.State.Running}}' "$container" 2>/dev/null)" = "true" ]; }

echo "── setup ───────────────────────────────────────────────────"
"$SBX" create "$NAME" --spec "$SPEC_DIR/sandbox.json" >/dev/null 2>&1 || { echo "create failed" >&2; exit 1; }
"$SBX" exec "$NAME" redis redis-cli set survived yes >/dev/null 2>&1
eval "$("$SBX" env "$NAME" --spec "$SPEC_DIR/sandbox.json")"
ok "sandbox created on :$REDIS_PORT"

start_daemon
ok "daemon running (pid $DAEMON)"

echo
echo "── 1. killing the daemon must not stop a live sandbox ──────"
redis-cli -h 127.0.0.1 -p "$REDIS_PORT" ping >/dev/null 2>&1
running && ok "sandbox awake before the kill" || bad "sandbox was not awake"

kill -9 "$DAEMON" 2>/dev/null
sleep 3

if running; then
  ok "sandbox still running after SIGKILL - the daemon took nothing with it"
else
  bad "killing the daemon stopped the sandbox"
fi

echo
echo "── 2. a restarted daemon re-adopts what it never created ───"
start_daemon

if nc -z 127.0.0.1 "$REDIS_PORT" 2>/dev/null; then
  ok "restarted daemon is fronting :$REDIS_PORT again"
else
  bad "restarted daemon never re-adopted the sandbox"
fi

echo
echo "── 3. asleep across a restart, and still wakes ─────────────"
waited=0
while running && [ "$waited" -lt 60 ]; do sleep 2; waited=$((waited + 2)); done
running && bad "never slept" || ok "slept after ${waited}s"

kill -9 "$DAEMON" 2>/dev/null
sleep 2
ok "daemon killed while the sandbox was asleep"

start_daemon

t0=$(python3 -c 'import time;print(int(time.time()*1000))')
got=$(redis-cli -h 127.0.0.1 -p "$REDIS_PORT" get survived 2>&1)
t1=$(python3 -c 'import time;print(int(time.time()*1000))')

if [ "$got" = "yes" ]; then
  ok "woken by a new daemon in $((t1 - t0))ms, data intact"
else
  bad "expected 'yes', got '$got'"
fi

echo
echo "───────────────────────────────────────────────────────────"
echo "  $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
