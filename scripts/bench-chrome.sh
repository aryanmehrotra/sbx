#!/usr/bin/env bash
# Headless Chrome wake (examples/browser), woken by a plain CDP GET /json/version.
# Cold and warm alternate: odd runs drop the page cache first (cold), even runs do not (warm).
#
# Needs a built ./sbx at the repo root and docker. Used for the v0.14.0 figures in docs/BENCHMARKS.md.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX=$ROOT/sbx; RUNS=${1:-10}; N=chromeb$$
. "$ROOT/scripts/lib/measure.sh"
SPEC=$ROOT/examples/browser/sandbox.json
cleanup() { [ -n "${D:-}" ] && kill $D 2>/dev/null; $SBX rm $N >/dev/null 2>&1; }; trap cleanup EXIT
t0=$(measure_ms); $SBX create $N --spec $SPEC >/dev/null || exit 1; t1=$(measure_ms); echo "create $((t1-t0)) ms"
eval "$($SBX env $N --spec $SPEC)"
$SBX serve --idle 5s --refresh 2s >/dev/null 2>&1 & D=$!; sleep 3
cold=""; warm=""
for i in $(seq 1 $RUNS); do
  w=0; while [ "$(docker inspect -f '{{.State.Running}}' sbx-$N-chrome)" = true ]; do sleep 1; w=$((w+1)); [ $w -gt 90 ] && continue 2; done
  if [ $((i%2)) = 1 ]; then sync; echo 3 > /proc/sys/vm/drop_caches || echo "no drop_caches"; kind=cold; else kind=warm; fi
  t0=$(measure_ms); out=$(curl -s --max-time 60 http://127.0.0.1:$CDP_PORT/json/version); t1=$(measure_ms)
  echo "$out" | grep -q Browser || { echo "run $i $kind FAILED"; continue; }
  d=$((t1-t0)); echo "run $i $kind $d ms"
  [ $kind = cold ] && cold="$cold $d" || warm="$warm $d"
done
for k in cold warm; do xs=$(eval echo \$$k | tr ' ' '\n' | grep -v '^$')
  echo "$k n=$(echo "$xs"|measure_stat n) median=$(echo "$xs"|measure_stat median) min=$(echo "$xs"|measure_stat min) max=$(echo "$xs"|measure_stat max)"; done
# harness floor: the same curl against the awake container
curl -s http://127.0.0.1:$CDP_PORT/json/version >/dev/null
f=""; for i in 1 2 3 4 5; do t0=$(measure_ms); curl -s http://127.0.0.1:$CDP_PORT/json/version >/dev/null; t1=$(measure_ms); f="$f $((t1-t0))"; done
echo "floor (awake, same client) ms:$f"
