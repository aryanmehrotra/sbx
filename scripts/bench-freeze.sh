#!/usr/bin/env bash
# Thaw latency: how long a caller waits when it connects to a sandbox that went idle with
# "on_idle": "freeze" (docker pause - memory and processes kept) instead of the default stop.
#
#   scripts/bench-freeze.sh [runs]     # default 20
#
# Needs a built ./sbx at the repo root, docker, and redis-cli on the host. Used for the
# v0.14.0 freeze figures in docs/BENCHMARKS.md. No other sbx daemon may be running: this
# script starts its own, and two daemons would race to freeze and thaw the same container.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX=$ROOT/sbx; RUNS=${1:-20}
# shellcheck source=lib/measure.sh
. "$ROOT/scripts/lib/measure.sh"
NAME="freezeb-$$"; C="sbx-${NAME}-redis"
W=$(mktemp -d)

[ -x "$SBX" ] || { echo "bench-freeze: build first: go build -o sbx ." >&2; exit 1; }

# The spec bench.sh uses, plus on_idle: the only variable is what "asleep" means.
cat > "$W/spec.json" <<'J'
{ "version": 1, "services": { "redis": { "image": "redis:7-alpine", "ports": [6379],
  "health": "redis-cli ping", "on_idle": "freeze" } }, "exports": { "REDIS_PORT": "redis:6379" } }
J

cleanup() { [ -n "${DAEMON:-}" ] && kill "$DAEMON" 2>/dev/null; "$SBX" rm "$NAME" >/dev/null 2>&1; rm -rf "$W"; }
trap cleanup EXIT

measure_conditions "$SBX"
"$SBX" create "$NAME" --spec "$W/spec.json" >/dev/null || { echo "bench-freeze: create failed" >&2; exit 1; }
eval "$("$SBX" env "$NAME" --spec "$W/spec.json")"

# A short idle so a run takes minutes. The window changes how long we wait for a freeze,
# not how long a thaw takes.
"$SBX" serve --idle 3s --refresh 1s >/dev/null 2>&1 &
DAEMON=$!
sleep 2

ping_ms() { # one timed redis-cli ping; prints ms, or FAIL
  local t0 t1 out
  t0=$(measure_ms); out=$(redis-cli -h 127.0.0.1 -p "$REDIS_PORT" ping 2>&1); t1=$(measure_ms)
  [ "$out" = PONG ] && echo $((t1 - t0)) || echo "FAIL:$out"
}

thaw=""; floor=""; pairs=""
for i in $(seq 1 "$RUNS"); do
  # Wait for the daemon to freeze it, so every sample starts paused. Paused, not stopped:
  # a stopped container here would mean on_idle was ignored, and the sample would be a
  # cold wake mislabelled as a thaw.
  waited=0
  until [ "$(docker inspect -f '{{.State.Paused}}' "$C" 2>/dev/null)" = true ]; do
    [ "$(docker inspect -f '{{.State.Running}}' "$C" 2>/dev/null)" = true ] ||
      { echo "bench-freeze: $C stopped instead of freezing" >&2; exit 1; }
    sleep 0.2; waited=$((waited + 1))
    [ "$waited" -gt 150 ] && { echo "run $i: never froze, skipping" >&2; continue 2; }
  done
  d=$(ping_ms); echo "run $i thaw $d ms"
  case $d in FAIL*) continue ;; esac
  thaw="$thaw $d"
  # Harness floor: the same ping against the now-awake box, well inside the idle window.
  # It is the process spawn + measure_ms cost that sits inside every thaw sample too.
  f=$(ping_ms); case $f in FAIL*) ;; *) floor="$floor $f"; pairs="$pairs $d:$f" ;; esac
done

report() { # label, samples
  local ys; ys=$(echo "$2" | tr ' ' '\n' | grep -v '^$')
  echo "$1 n=$(echo "$ys"|measure_stat n) median=$(echo "$ys"|measure_stat median) p90=$(echo "$ys"|measure_stat p90) min=$(echo "$ys"|measure_stat min) max=$(echo "$ys"|measure_stat max) (ms)"
}
report "thaw (ping, frozen)" "$thaw"
report "floor (ping, awake)" "$floor"
# Paired per run, not median minus median: the client spawn cost sits in both samples.
report "thaw - floor, paired" "$(echo "$pairs" | tr ' ' '\n' | measure_pairs)"
