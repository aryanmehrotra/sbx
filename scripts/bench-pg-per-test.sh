#!/usr/bin/env bash
# "A Postgres per test": time until a fresh or sleeping Postgres answers its first query.
#
#   scripts/bench-pg-per-test.sh [RUNS]          # default 10 per contender
#   CONTENDERS=tc,compose scripts/bench-pg-per-test.sh 5
#
# Contenders, interleaved and rotated every round so none inherits a busy engine:
#   tc       Python testcontainers: PostgresContainer(...).start() -> first query, in-process
#   compose  docker compose up -d --wait (healthcheck = the template's) -> first query
#   with     sbx with <new> --template postgres -- <first query>, create -> query and whole run
#   wake     an existing sbx sandbox after `sbx sleep` -> first query through the daemon
# Every contender uses the same image (examples/postgres/sandbox.json, digest-pinned), the same
# app/app/app credentials and the same client: the host's psql running `select 1`. A sample
# counts only when psql prints 1.
#
# Needs a built ./sbx, docker, docker compose, psql, and `pip install testcontainers[postgres]`
# for tc. Starts `sbx serve` if none is running. Used for docs/BENCHMARKS.md's head to head.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX=$ROOT/sbx; RUNS=${1:-10}
CONTENDERS="${CONTENDERS:-tc,compose,with,wake}"
. "$ROOT/scripts/lib/measure.sh"
[ -x "$SBX" ] || { echo "bench: build first: go build -o sbx ." >&2; exit 1; }
command -v psql >/dev/null || { echo "bench: needs psql on PATH (apt-get install postgresql-client)" >&2; exit 1; }

IMAGE=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["services"]["postgres"]["image"])' "$ROOT/examples/postgres/sandbox.json")
W=$(mktemp -d); P=pgpt$$; WAKE=$P-wake; DAEMON=""
docker image inspect "$IMAGE" >/dev/null 2>&1 || docker pull -q "$IMAGE" >/dev/null

cleanup() {
  docker compose -p "$P" -f "$W/compose.yaml" down -v >/dev/null 2>&1
  "$SBX" rm "$WAKE" >/dev/null 2>&1
  for i in $(seq 1 "$RUNS"); do "$SBX" rm "$P-with-$i" >/dev/null 2>&1; done
  # testcontainers' reaper is off (below), so anything a crashed round left is removed here.
  docker ps -aq --filter "label=pgpt=$P" | xargs docker rm -f >/dev/null 2>&1
  [ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
  rm -rf "$W"
}
trap cleanup EXIT

# q PORT - the one client every contender is judged by.
q() { PGPASSWORD=app psql -h 127.0.0.1 -p "$1" -U app -d app -tAc 'select 1' 2>/dev/null | grep -q '^1$'; }

# The compose side gets the sbx template's health check at sbx's own pace (300 ms interval,
# 60 s start period, internal/provider/docker_health.go), so --wait is not slowed by a default
# 30 s interval that nobody would ship for this. start_interval too: without it Docker 25+ probes
# every 5 s inside the start period, and compose measured 5.9 s instead of 2.4.
CPORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
cat > "$W/compose.yaml" <<Y
services:
  postgres:
    image: $IMAGE
    environment: { POSTGRES_USER: app, POSTGRES_PASSWORD: app, POSTGRES_DB: app }
    ports: ["127.0.0.1:$CPORT:5432"]
    labels: { pgpt: "$P" }
    healthcheck:
      test: ["CMD-SHELL", "psql -U app -d app -c 'select 1'"]
      interval: 300ms
      timeout: 2s
      retries: 3
      start_period: 60s
      start_interval: 300ms
Y

# Timed inside Python, from start() to the first successful query, so the interpreter's own
# start and imports are not charged to testcontainers: a test suite pays those once. The reaper
# (Ryuk) is off for the same reason: it starts once per session, not per database.
cat > "$W/tc.py" <<'PY'
import subprocess, sys, time
from testcontainers.postgres import PostgresContainer
c = PostgresContainer(sys.argv[1], username="app", password="app", dbname="app").with_kwargs(labels={"pgpt": sys.argv[2]})
t0 = time.time()
c.start()
try:
    port = c.get_exposed_port(5432)
    ok = subprocess.run(["psql", "-h", "127.0.0.1", "-p", str(port), "-U", "app", "-d", "app", "-tAc", "select 1"],
                        env={"PGPASSWORD": "app", "PATH": "/usr/bin:/bin:/usr/local/bin"}, capture_output=True, text=True).stdout.strip() == "1"
    t1 = time.time()
    print(int((t1 - t0) * 1000) if ok else "FAIL")
finally:
    c.stop()
PY
case ",$CONTENDERS," in *,tc,*)
  python3 -c 'import testcontainers.postgres' 2>/dev/null ||
    { echo "bench: tc needs: pip install 'testcontainers[postgres]' psycopg2-binary" >&2; exit 1; } ;;
esac

# Matched on the program and its first argument: pgrep -f would also match any shell whose
# command line merely mentions "sbx serve".
if ! ps -axo comm=,args= | awk '$1 == "sbx" && $3 == "serve" {f=1} END {exit !f}'; then "$SBX" serve >/dev/null 2>&1 & DAEMON=$!; sleep 3; fi
case ",$CONTENDERS," in *,wake,*)
  "$SBX" create "$WAKE" --template postgres >/dev/null 2>&1 || { echo "bench: sbx create $WAKE failed" >&2; exit 1; }
  eval "$("$SBX" env "$WAKE")"; WPORT=$PGPORT ;;
esac

measure_conditions "$SBX"
echo "image $IMAGE; contenders $CONTENDERS; $RUNS rounds"

one() { # contender round -> prints "<contender> <ms>[ <ms-total>]" or "<contender> FAIL ..."
  local t0 t1 t2 out
  case "$1" in
    tc) out=$(TESTCONTAINERS_RYUK_DISABLED=true python3 "$W/tc.py" "$IMAGE" "$P" 2>"$W/tc.err" | tail -1)
        case "$out" in ''|*[!0-9]*) echo "tc FAIL $(tail -1 "$W/tc.err")" ;; *) echo "tc $out" ;; esac ;;
    compose)
        t0=$(measure_ms)
        docker compose -p "$P" -f "$W/compose.yaml" up -d --wait >/dev/null 2>&1 && q "$CPORT"; ok=$?
        t1=$(measure_ms)
        docker compose -p "$P" -f "$W/compose.yaml" down -v >/dev/null 2>&1
        [ $ok = 0 ] && echo "compose $((t1 - t0))" || echo "compose FAIL" ;;
    with)
        # The query prints its own timestamp, so create -> first query and the whole run (which
        # includes sbx with's removal) come from one invocation.
        t0=$(measure_ms)
        # To a file, because sbx with prints its own progress on stdout around the command's.
        rm -f "$W/with.ts"
        TS="$W/with.ts" "$SBX" with "$P-with-$2" --template postgres -- sh -c \
          'PGPASSWORD=app psql -h 127.0.0.1 -p "$PGPORT" -U app -d app -tAc "select 1" | grep -q "^1$" && python3 -c "import time;print(int(time.time()*1000))" > "$TS"' >/dev/null 2>&1
        t2=$(measure_ms); out=$(cat "$W/with.ts" 2>/dev/null)
        case "$out" in ''|*[!0-9]*) echo "with FAIL" ;; *) echo "with $((out - t0)) $((t2 - t0))" ;; esac ;;
    wake)
        "$SBX" sleep "$WAKE" >/dev/null 2>&1
        # Verified asleep: no running container for it, or the sample would be an awake query.
        if [ -n "$(docker ps -q --filter "name=^sbx-$WAKE-postgres$")" ]; then echo "wake FAIL still running"; return; fi
        t0=$(measure_ms); q "$WPORT"; ok=$?; t1=$(measure_ms)
        [ $ok = 0 ] && echo "wake $((t1 - t0))" || echo "wake FAIL" ;;
  esac
}

LIST=$(echo "$CONTENDERS" | tr ',' ' ')
for r in $(seq 1 "$RUNS"); do
  # Rotate the order by one each round.
  set -- $LIST; k=$(( (r - 1) % $# )); while [ "$k" -gt 0 ]; do x=$1; shift; set -- "$@" "$x"; k=$((k - 1)); done
  for c in "$@"; do echo "round $r $(one "$c" "$r")"; sleep 2; done
done | tee "$W/raw.txt"

echo "── distribution (ms) ──"
for c in $LIST; do
  ys=$(awk -v c="$c" '$3 == c && $4 ~ /^[0-9]+$/ {print $4}' "$W/raw.txt")
  echo "$c n=$(echo "$ys" | measure_stat n) median=$(echo "$ys" | measure_stat median) p90=$(echo "$ys" | measure_stat p90) min=$(echo "$ys" | measure_stat min) max=$(echo "$ys" | measure_stat max) failed=$(awk -v c="$c" '$3 == c && $4 == "FAIL"' "$W/raw.txt" | wc -l | tr -d ' ')"
done
ys=$(awk '$3 == "with" && $5 ~ /^[0-9]+$/ {print $5}' "$W/raw.txt")
echo "with (whole run, incl. removal) n=$(echo "$ys" | measure_stat n) median=$(echo "$ys" | measure_stat median) min=$(echo "$ys" | measure_stat min) max=$(echo "$ys" | measure_stat max)"
