#!/usr/bin/env bash
# Wake latency, measured properly.
#
#   scripts/bench.sh [runs]        # default 10
#
# The Go benchmark next door measures the steady-state proxy tax. This measures the other
# number: how long a caller waits when it connects to a sandbox that is asleep.
#
# It reports a distribution, not a number. Every wake figure quoted before this script
# existed was a single run, and the spread here is wide enough that a single run says more
# about what else the machine was doing than about the sandbox. Machine conditions are
# recorded alongside the results for the same reason — a wake on an idle laptop and a wake
# on one that is paging are not the same measurement, and the numbers should say which.
set -uo pipefail

RUNS="${1:-10}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
# shellcheck source=lib/measure.sh
. "$ROOT/scripts/lib/measure.sh"
NAME="bench-$$"
SPEC="$(mktemp -d)/sandbox.json"

[ -x "$SBX" ] || { echo "bench: build first: go build -o sbx ." >&2; exit 1; }

# Redis, not MySQL: this is measuring the wake path, and a heavier workload only adds its own
# startup variance on top of what is being compared.
cat > "$SPEC" <<'EOF'
{
  "version": 1,
  "services": {
    "redis": {
      "image": "redis:7-alpine",
      "ports": [6379],
      "health": "redis-cli ping"
    }
  },
  "exports": { "REDIS_PORT": "redis:6379" }
}
EOF

cleanup() {
  [ -n "${DAEMON:-}" ] && kill "$DAEMON" 2>/dev/null
  "$SBX" rm "$NAME" >/dev/null 2>&1
}
trap cleanup EXIT

measure_conditions "$SBX"
echo

echo "── setup ───────────────────────────────────────────────────"
t0=$(measure_ms)
"$SBX" create "$NAME" --spec "$SPEC" >/dev/null || { echo "bench: create failed" >&2; exit 1; }
t1=$(measure_ms)
printf '  create         %s ms  (image pull, health wait, init — once)\n' "$((t1 - t0))"

eval "$("$SBX" env "$NAME" --spec "$SPEC")"

# A short idle so a run takes minutes rather than an hour. The window does not affect how
# long a wake takes; it only affects how long this script waits to trigger one.
"$SBX" serve --idle 5s --refresh 2s >/dev/null 2>&1 &
DAEMON=$!
sleep 3
echo

echo "── wake latency, ${RUNS} runs ──────────────────────────────────"
samples=""

for i in $(seq 1 "$RUNS"); do
  # Wait for the daemon to put it to sleep, so every sample starts from the same state.
  waited=0
  while [ "$(docker inspect -f '{{.State.Running}}' "sbx-${NAME}-redis" 2>/dev/null)" = "true" ]; do
    sleep 1
    waited=$((waited + 1))
    [ "$waited" -gt 60 ] && { echo "  run $i: never slept, skipping" >&2; continue 2; }
  done

  t0=$(measure_ms)
  out=$(redis-cli -h 127.0.0.1 -p "$REDIS_PORT" ping 2>&1)
  t1=$(measure_ms)

  if [ "$out" != "PONG" ]; then
    printf '  run %-3s        FAILED: %s\n' "$i" "$out"
    continue
  fi

  d=$((t1 - t0))
  samples="$samples $d"
  printf '  run %-3s        %s ms\n' "$i" "$d"
done

echo
echo "── distribution ────────────────────────────────────────────"
xs=$(echo "$samples" | tr ' ' '\n' | grep -v '^$')
if [ -z "$xs" ]; then
  echo "  no successful samples"
  exit 1
fi
printf '  n              %s\n'    "$(echo "$xs" | measure_stat n)"
printf '  min            %s ms\n' "$(echo "$xs" | measure_stat min)"
printf '  median         %s ms\n' "$(echo "$xs" | measure_stat median)"
printf '  p90            %s ms\n' "$(echo "$xs" | measure_stat p90)"
printf '  max            %s ms\n' "$(echo "$xs" | measure_stat max)"
printf '  stdev          %s ms\n' "$(echo "$xs" | measure_stat stdev)"

