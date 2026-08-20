#!/usr/bin/env bash
# Data safety under interruption, end to end, against a real postgres.
#
#   scripts/interrupt-e2e.sh [rounds]
#
# recovery.sh kills the daemon. This kills the destructive data operations themselves - fork,
# rm, gc, and the copy that fork runs - at the worst possible moment, and asserts the one thing
# that must never break: the SOURCE a snapshot was made from is not corrupted, and no fork
# silently survives with wrong or partial data. CopyVolume has shipped two data-loss bugs (see
# DECISIONS.md and copyvolume_docker_test.go); those are fixed and unit-tested. This is the tier
# above: not "does the copy replace correctly" but "what if the copy is cut in half".
#
# Two interruption vectors, because sbx delegates the copy to a container:
#   A. kill the sbx CLI mid-operation. The container it spawned keeps running under dockerd, so
#      this is the "the caller died, the daemon carried on" case - it should never corrupt.
#   B. kill the copy CONTAINER mid-write. This is the real torn write - a dockerd crash, an
#      OOM-killed copier, a lost node. The source is mounted read-only, so it must survive; a
#      half-copied destination must be caught, never served as if whole.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
ROUNDS="${1:-4}"
SEED="ie-seed-$$"
SNAP="ie-golden-$$"
WORK="$(mktemp -d)"

[ -x "$SBX" ] || { echo "interrupt-e2e: build first: go build -o sbx ." >&2; exit 1; }

pass=0
fail=0
ok()  { pass=$((pass + 1)); echo "  ✓ $1"; }
bad() { fail=$((fail + 1)); echo "  ✗ $1"; [ -n "${2:-}" ] && echo "      $2"; }

cleanup() {
  "$SBX" rm "$SEED" >/dev/null 2>&1
  for f in $(docker ps -aq --filter "name=sbx-ie-fork-" 2>/dev/null); do docker rm -f "$f" >/dev/null 2>&1; done
  "$SBX" gc --snapshots --force >/dev/null 2>&1
  docker images -q "sbx-snap-$SNAP-*" 2>/dev/null | xargs -r docker rmi -f >/dev/null 2>&1
  docker volume ls -q --filter "name=sbx-snapvol-$SNAP" 2>/dev/null | xargs -r docker volume rm >/dev/null 2>&1
  docker volume ls -q --filter "name=sbx-ie-fork-" 2>/dev/null | xargs -r docker volume rm >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

# The checksum of a volume's file contents. This is the invariant: the golden source's must not
# move, whatever we do to operations that read from it.
vol_sum() {
  docker run --rm -v "$1":/v:ro alpine:3 \
    sh -c 'find /v -type f 2>/dev/null | sort | xargs -r sha256sum 2>/dev/null | sha256sum' 2>/dev/null | awk '{print $1}'
}

query() { "$SBX" exec "$1" postgres psql -U app -d app -tAc "$2" 2>/dev/null | tr -d ' \n'; }

echo "── seed a golden snapshot ──────────────────────────────────"
cp "$ROOT/examples/postgres/sandbox.json" "$WORK/sandbox.json"

if ! "$SBX" create "$SEED" --spec "$WORK/sandbox.json" >/dev/null 2>&1; then
  bad "create the seed sandbox"; echo; echo "  $pass passed, $fail failed"; exit 1
fi

# A known row, plus a chunky blob so the volume copy takes long enough to catch mid-write.
"$SBX" exec "$SEED" postgres psql -U app -d app \
  -c "create table t(v text)" \
  -c "insert into t values ('golden')" \
  -c "create table blob(b bytea)" \
  -c "insert into blob select repeat('x', 8*1024*1024)::bytea from generate_series(1,6)" \
  -c "checkpoint" >/dev/null 2>&1

[ "$(query "$SEED" 'select v from t')" = "golden" ] \
  && ok "seed holds the known row" || bad "seed missing the known row"

"$SBX" snapshot "$SEED" "$SNAP" >/dev/null 2>&1
GOLDEN_VOL="sbx-snapvol-$SNAP-postgres"
GOLDEN_SUM="$(vol_sum "$GOLDEN_VOL")"
[ -n "$GOLDEN_SUM" ] && ok "golden snapshot volume has a checksum ($GOLDEN_SUM)" \
  || { bad "golden snapshot volume has no checksum"; echo; echo "  $pass passed, $fail failed"; exit 1; }

golden_intact() { # label
  local now; now="$(vol_sum "$GOLDEN_VOL")"
  [ "$now" = "$GOLDEN_SUM" ] && ok "golden source intact after $1" \
    || bad "GOLDEN SOURCE CORRUPTED after $1" "was $GOLDEN_SUM, now $now"
}

echo
echo "── A · kill the sbx CLI mid-operation, $ROUNDS rounds ──────────"
# The container keeps running under dockerd, so the invariant is: the source never moves, and a
# fork that ends up present is either correct or cleanly gone - never healthy with wrong data.
for i in $(seq 1 "$ROUNDS"); do
  fk="ie-fork-$$-$i"
  "$SBX" fork "$SNAP" "$fk" --spec "$WORK/sandbox.json" >/dev/null 2>&1 &
  pid=$!
  # land somewhere inside the op: create, then the volume copy
  sleep "0.$((RANDOM % 9))"
  kill -9 "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  golden_intact "kill -9 sbx fork (round $i)"

  # If the fork came up, it must hold the golden row - never empty, never partial-but-serving.
  if query "$fk" 'select 1' 2>/dev/null | grep -q 1; then
    [ "$(query "$fk" 'select v from t')" = "golden" ] \
      && ok "  fork that survived the kill has correct data" \
      || bad "  fork survived with WRONG data - the bug shape" "$(query "$fk" 'select v from t')"
  fi
  "$SBX" rm "$fk" >/dev/null 2>&1 &
  kpid=$!
  sleep "0.$((RANDOM % 5))"
  kill -9 "$kpid" 2>/dev/null; wait "$kpid" 2>/dev/null
  golden_intact "kill -9 sbx rm (round $i)"
done

echo
echo "── B · kill the copy CONTAINER mid-write ───────────────────"
# The real torn write. Launch a fork, race to docker-kill the alpine copier while it runs, and
# assert the read-only source survived and no half-copy is served as whole.
for i in $(seq 1 "$ROUNDS"); do
  fk="ie-fork-b-$$-$i"
  "$SBX" fork "$SNAP" "$fk" --spec "$WORK/sandbox.json" >/dev/null 2>&1 &
  pid=$!
  killed=""
  for _ in $(seq 1 400); do   # ~4s of tight polling
    cid="$(docker ps -q --filter ancestor=alpine:3 --filter volume="$GOLDEN_VOL" 2>/dev/null | head -1)"
    [ -z "$cid" ] && cid="$(docker ps -q --filter ancestor=alpine:3 2>/dev/null | head -1)"
    if [ -n "$cid" ]; then docker kill "$cid" >/dev/null 2>&1 && killed="yes"; break; fi
    sleep 0.01
  done
  wait "$pid" 2>/dev/null
  golden_intact "docker kill the copier (round $i)"
  [ -n "$killed" ] && ok "  caught and killed the copy container mid-write" \
    || echo "      (copier finished before it could be caught - copy is fast; still checked the source)"

  if query "$fk" 'select 1' 2>/dev/null | grep -q 1; then
    [ "$(query "$fk" 'select v from t')" = "golden" ] \
      && ok "  fork after a killed copier has correct data" \
      || bad "  fork after a killed copier serves WRONG/partial data" "$(query "$fk" 'select v from t')"
  fi
  "$SBX" rm "$fk" >/dev/null 2>&1
done

echo
echo "── recoverability ──────────────────────────────────────────"
# After all that abuse, a clean fork from golden must still produce correct data - the
# interruptions must not have wedged the snapshot or left the system unable to fork.
FINAL="ie-final-$$"
if "$SBX" fork "$SNAP" "$FINAL" --spec "$WORK/sandbox.json" >/dev/null 2>&1; then
  got=""
  for _ in $(seq 1 30); do   # postgres restarts onto the restored data dir; give it a moment
    got="$(query "$FINAL" 'select v from t')"
    [ "$got" = "golden" ] && break
    sleep 0.5
  done
  [ "$got" = "golden" ] \
    && ok "a clean fork from golden after all interruptions has correct data" \
    || bad "a clean fork after interruptions has wrong data - the snapshot was wedged" "read: '$got'"
else
  bad "could not fork from golden after the interruptions - the system was left wedged"
fi
"$SBX" rm "$FINAL" >/dev/null 2>&1
golden_intact "the whole run"

echo
echo "  $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
