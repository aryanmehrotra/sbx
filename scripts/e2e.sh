#!/usr/bin/env bash
# Several sandboxes at once, which is the entire premise and had never been run.
#
#   scripts/e2e.sh [count]        # default 3
#
# `sbx selftest` proves one sandbox works. That is not the claim. The claim is that a dozen
# branches can each have their own, sleeping independently, without colliding - and the
# failure that would matter most is the quiet one: sandbox A's client being handed sandbox
# B's data. Nothing until now would have noticed that.
#
# So every sandbox writes a value only it should have, and at the end each is asked for it
# through its own port. A mix-up shows up as the wrong branch's answer, not as an error.
set -uo pipefail

COUNT="${1:-3}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
SPEC_DIR="$(mktemp -d)"
NAMES=""

[ -x "$SBX" ] || { echo "e2e: build first: go build -o sbx ." >&2; exit 1; }

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
  [ -n "${DAEMON:-}" ] && kill "$DAEMON" 2>/dev/null
  for n in $NAMES; do "$SBX" rm "$n" >/dev/null 2>&1; done
  rm -rf "$SPEC_DIR"
}
trap cleanup EXIT

echo "── creating ${COUNT} sandboxes ─────────────────────────────"

for i in $(seq 1 "$COUNT"); do
  n="e2e-$$-$i"
  NAMES="$NAMES $n"

  if "$SBX" create "$n" --spec "$SPEC_DIR/sandbox.json" >/dev/null 2>&1; then
    ok "created $n"
  else
    bad "create $n"; exit 1
  fi
done

echo
echo "── each got its own port block ─────────────────────────────"
ports=""

for n in $NAMES; do
  p=$(eval "$("$SBX" env "$n" --spec "$SPEC_DIR/sandbox.json")"; echo "$REDIS_PORT")
  ports="$ports $p"
  echo "  $n → :$p"
done

uniq_count=$(echo "$ports" | tr ' ' '\n' | grep -v '^$' | sort -u | wc -l | tr -d ' ')
total=$(echo "$ports" | tr ' ' '\n' | grep -v '^$' | wc -l | tr -d ' ')

if [ "$uniq_count" = "$total" ]; then
  ok "all $total ports distinct"
else
  bad "port collision: $total sandboxes share $uniq_count ports"
fi

echo
echo "── writing a value only each sandbox should have ───────────"
i=0
for n in $NAMES; do
  i=$((i + 1))
  if "$SBX" exec "$n" redis redis-cli set owner "$n" >/dev/null 2>&1; then
    ok "$n wrote its own marker"
  else
    bad "$n could not write"
  fi
done

# Short idle so the run takes minutes rather than an afternoon.
"$SBX" serve --idle 5s --refresh 2s >/dev/null 2>&1 &
DAEMON=$!

echo
echo "── all of them sleep ───────────────────────────────────────"
waited=0
while [ "$waited" -lt 90 ]; do
  awake=$(docker ps --filter "label=sbx.sandbox" --format '{{.Names}}' 2>/dev/null | grep -c "e2e-$$-" || true)
  [ "$awake" = "0" ] && break
  sleep 2
  waited=$((waited + 2))
done

if [ "$awake" = "0" ]; then
  ok "all $COUNT asleep after ${waited}s (0 bytes resident)"
else
  bad "$awake still awake after 90s"
fi

echo
echo "── woken at the same time, each answering for itself ───────"
tmp="$SPEC_DIR/results"
mkdir -p "$tmp"

i=0
clients=""

for n in $NAMES; do
  i=$((i + 1))
  p=$(echo "$ports" | tr ' ' '\n' | grep -v '^$' | sed -n "${i}p")
  # Concurrently on purpose: a shared reaper, a shared docker daemon and N wakes at once is
  # the state a machine with several branches open is actually in.
  #
  # No -t: Homebrew's redis-cli takes it as a timeout and Ubuntu's redis-tools rejects it
  # outright, which turned every read here into "Unrecognized option" on CI while the
  # sandboxes themselves were fine. None is needed - sbx holds the connection open during a
  # wake, so the client is connected and waiting for a reply, not waiting to connect.
  ( redis-cli -h 127.0.0.1 -p "$p" get owner > "$tmp/$n" 2>&1 ) &
  clients="$clients $!"
done

# Only these, never a bare `wait`: the daemon is also a background job of this shell, so
# waiting on everything waits on something that never exits. That mistake hung this script
# for ten minutes looking exactly like a bug in the thing it was testing.
# shellcheck disable=SC2086
wait $clients

for n in $NAMES; do
  got=$(cat "$tmp/$n" 2>/dev/null)

  if [ "$got" = "$n" ]; then
    ok "$n answered with its own data"
  else
    bad "$n answered with '$got' - cross-talk or lost state"
  fi
done

echo
echo "───────────────────────────────────────────────────────────"
echo "  $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
