#!/usr/bin/env bash
# sbx create time for the redis spec bench.sh uses, image present; n runs, each followed by rm.
#
# Needs a built ./sbx at the repo root and docker. Used for the v0.14.0 figures in docs/BENCHMARKS.md.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX=$ROOT/sbx; RUNS=${1:-10}
. "$ROOT/scripts/lib/measure.sh"
W=$(mktemp -d); cat > $W/spec.json <<'J'
{ "version": 1, "services": { "redis": { "image": "redis:7-alpine", "ports": [6379],
  "health": "redis-cli ping" } }, "exports": { "REDIS_PORT": "redis:6379" } }
J
xs=""
for i in $(seq 1 $RUNS); do N=createb$$-$i
  t0=$(measure_ms); $SBX create $N --spec $W/spec.json >/dev/null 2>&1 || echo "run $i failed"; t1=$(measure_ms)
  echo "run $i $((t1-t0)) ms"; xs="$xs $((t1-t0))"; $SBX rm $N >/dev/null 2>&1; sleep 1
done
ys=$(echo $xs | tr ' ' '\n')
echo "create n=$(echo "$ys"|measure_stat n) median=$(echo "$ys"|measure_stat median) p90=$(echo "$ys"|measure_stat p90) min=$(echo "$ys"|measure_stat min) max=$(echo "$ys"|measure_stat max)"
# harness floor: measure_ms pair alone
f=""; for i in 1 2 3 4 5; do t0=$(measure_ms); t1=$(measure_ms); f="$f $((t1-t0))"; done; echo "measure_ms floor ms:$f"
