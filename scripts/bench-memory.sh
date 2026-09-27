#!/usr/bin/env bash
# daemon RSS: at rest (no sandboxes), fronting one sleeping redis sandbox, after a wake + 1000 PINGs.
# Sleeping-sandbox memory: whether any container of it is running (docker stats).
# Repeated ROUNDS times, each with a fresh daemon.
#
# Needs a built ./sbx at the repo root and docker. Used for the v0.14.0 figures in docs/BENCHMARKS.md.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX=$ROOT/sbx; ROUNDS=${1:-3}
W=$(mktemp -d); cat > $W/spec.json <<'J'
{ "version": 1, "services": { "redis": { "image": "redis:7-alpine", "ports": [6379],
  "health": "redis-cli ping" } }, "exports": { "REDIS_PORT": "redis:6379" } }
J
rss() { ps -o rss= -p "$1" | awk '{printf "%.1f", $1/1024}'; }
for r in $(seq 1 $ROUNDS); do
  N=memb$$r
  $SBX serve --idle 5s --refresh 2s >$W/d.log 2>&1 & D=$!
  sleep 8; rest=$(rss $D)
  $SBX create $N --spec $W/spec.json >/dev/null 2>&1 || { echo create failed; kill $D; exit 1; }
  eval "$($SBX env $N --spec $W/spec.json)"
  w=0; while [ "$(docker inspect -f '{{.State.Running}}' sbx-$N-redis)" = true ]; do sleep 1; w=$((w+1)); [ $w -gt 60 ] && break; done
  sleep 3; front=$(rss $D)
  running=$(docker ps --filter name=sbx-$N- --format '{{.Names}}' | wc -l)
  for _ in $(seq 1 1000); do echo PING; done | redis-cli -p $REDIS_PORT >/dev/null
  for _ in $(seq 1 50); do redis-cli -p $REDIS_PORT ping >/dev/null; done
  sleep 2; after=$(rss $D)
  echo "round $r  daemon_rest_MB $rest  fronting_one_asleep_MB $front  after_traffic_MB $after  containers_running_while_asleep $running"
  kill $D; wait $D 2>/dev/null; $SBX rm $N >/dev/null 2>&1
done
rm -rf $W
