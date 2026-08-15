#!/usr/bin/env bash
# Snapshot and fork, end to end, against a real postgres.
#
#   scripts/fork-e2e.sh
#
# This exists because the first implementation of snapshot was wrong in a way that looked
# right: `docker commit` does not capture mounted volumes, so both forks came up with a
# healthy server and an empty database. Nothing short of writing a row, forking, and reading
# it back in the fork would have caught it — a create that succeeds is not evidence.
#
# It also proves the forks are independent, which is the actual promise. One copy of the
# data shared by every fork would be worse than no fork at all: it looks like isolation and
# is not.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
SEED="fe-seed-$$"
A="fe-a-$$"
B="fe-b-$$"
SNAP="fe-snap-$$"
WORK="$(mktemp -d)"

[ -x "$SBX" ] || { echo "fork-e2e: build first: go build -o sbx ." >&2; exit 1; }

pass=0
fail=0

ok()  { pass=$((pass + 1)); echo "  ✓ $1"; }
bad() { fail=$((fail + 1)); echo "  ✗ $1"; [ -n "${2:-}" ] && echo "      $2"; }

cleanup() {
  "$SBX" rm "$SEED" >/dev/null 2>&1
  "$SBX" rm "$A" >/dev/null 2>&1
  "$SBX" rm "$B" >/dev/null 2>&1
  docker rmi -f "$(docker images -q "sbx-snap-$SNAP-*" 2>/dev/null)" >/dev/null 2>&1
  docker volume rm "$(docker volume ls -q --filter "name=sbx-snapvol-$SNAP" 2>/dev/null)" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

cp "$ROOT/examples/postgres/sandbox.json" "$WORK/sandbox.json"

query() { # sandbox, sql -> stdout
  "$SBX" exec "$1" postgres psql -U app -d app -tAc "$2" 2>/dev/null | tr -d ' \n'
}

echo "── seed ────────────────────────────────────────────────────"

if ! "$SBX" create "$SEED" --spec "$WORK/sandbox.json" >/dev/null 2>&1; then
  bad "create the sandbox to snapshot"
  echo; echo "  $pass passed, $fail failed"; exit 1
fi

"$SBX" exec "$SEED" postgres psql -U app -d app \
  -c "create table t(v text); insert into t values ('seeded')" >/dev/null 2>&1

[ "$(query "$SEED" 'select v from t')" = "seeded" ] \
  && ok "the seed sandbox holds the row" \
  || bad "the seed sandbox does not hold the row"

echo
echo "── snapshot and fork twice ─────────────────────────────────"

"$SBX" snapshot "$SEED" "$SNAP" >/dev/null 2>&1 \
  && ok "snapshot taken" \
  || bad "snapshot failed"

for s in "$A" "$B"; do
  "$SBX" fork "$SNAP" "$s" --spec "$WORK/sandbox.json" >/dev/null 2>&1 \
    && ok "forked $s" \
    || bad "forking $s failed"
done

echo
echo "── the fork carries the data ───────────────────────────────"

# The assertion the first implementation would have failed: a healthy server with an empty
# database looked exactly like success.
for s in "$A" "$B"; do
  got=$(query "$s" 'select v from t')
  [ "$got" = "seeded" ] \
    && ok "$s carries the seeded row" \
    || bad "$s does not carry the seeded row" "got [$got] — docker commit does not capture volumes"
done

echo
echo "── and the forks are independent ───────────────────────────"

"$SBX" exec "$A" postgres psql -U app -d app -c "insert into t values ('only-a')" >/dev/null 2>&1

a=$(query "$A" 'select count(*) from t')
b=$(query "$B" 'select count(*) from t')
seed=$(query "$SEED" 'select count(*) from t')

[ "$a" = "2" ] && ok "the fork that was written to has 2 rows" \
               || bad "the written fork has $a rows, want 2"

[ "$b" = "1" ] && ok "the other fork is untouched" \
               || bad "the other fork has $b rows, want 1 — the forks share a volume"

[ "$seed" = "1" ] && ok "the original is untouched" \
                  || bad "the original has $seed rows, want 1 — a fork wrote to its source"

echo
echo "───────────────────────────────────────────────────────────"
echo "  $pass passed, $fail failed"

[ "$fail" -eq 0 ]
