#!/usr/bin/env bash
# Memory held by N idle branch databases: always-on containers vs sbx sandboxes asleep.
#
#   scripts/bench-fleet-memory.sh [N]      # default 20; needs a built ./sbx at the repo root and docker
#
# The comparison the README makes is "twenty branch databases on one machine". The always-on
# side is what docker compose (or a hand-rolled `docker run`) gives you: every container keeps
# running whether anybody uses it or not. It runs the postgres template's own image and
# environment, waits for every one to accept connections, lets them settle, then sums what
# `docker stats` reports. The sbx side creates N sandboxes from the same template, waits until
# the daemon has put every one to sleep, and adds the daemon's RSS to whatever of theirs is
# still running - which should be nothing. Both sides are cleaned up, whatever happens.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX=$ROOT/sbx; N=${1:-20}; TAG=fleet$$; SETTLE=${SETTLE:-30}
[ -x "$SBX" ] || { echo "build it first: go build -o sbx ."; exit 1; }

IMG=$(python3 -c "import json;print(json.load(open('$ROOT/examples/postgres/sandbox.json'))['services']['postgres']['image'])")
D=""
cleanup() {
  docker ps -aq --filter "label=bench=$TAG" | xargs -r docker rm -f >/dev/null 2>&1
  for i in $(seq 1 "$N"); do "$SBX" rm "$TAG-$i" >/dev/null 2>&1; done
  [ -n "$D" ] && kill "$D" 2>/dev/null
}
trap cleanup EXIT

# docker stats prints "23.5MiB / 15.6GiB"; this turns the first half into MB (10^6 bytes).
to_mb() { python3 -c "
import re,sys
t=0.0
for l in sys.stdin:
    m=re.match(r'\s*([\d.]+)\s*([KMG]i?B|B)',l)
    if not m: continue
    v,u=float(m.group(1)),m.group(2)
    t+=v*{'B':1,'KB':1e3,'MB':1e6,'GB':1e9,'KiB':1024,'MiB':1024**2,'GiB':1024**3}[u]
print('%.1f'%(t/1e6))"; }

echo "always-on: $N postgres containers, $IMG"
docker pull -q "$IMG" >/dev/null
for i in $(seq 1 "$N"); do
  docker run -d --label "bench=$TAG" -e POSTGRES_USER=app -e POSTGRES_PASSWORD=app \
    -e POSTGRES_DB=app "$IMG" >/dev/null || { echo "docker run $i failed"; exit 1; }
done
for c in $(docker ps -q --filter "label=bench=$TAG"); do
  for _ in $(seq 1 60); do docker exec "$c" pg_isready -q -U app 2>/dev/null && break; sleep 1; done
done
sleep "$SETTLE"
ids=$(docker ps -q --filter "label=bench=$TAG")
# shellcheck disable=SC2086 # one container id per word, on purpose (bash 3.2 has no mapfile)
on=$(docker stats --no-stream --format '{{.MemUsage}}' $ids | to_mb)
running_on=$(docker ps -q --filter "label=bench=$TAG" | wc -l)
echo "  $running_on running, $on MB total"
docker ps -aq --filter "label=bench=$TAG" | xargs -r docker rm -f >/dev/null

echo "sbx: $N sandboxes from --template postgres, left idle"
"$SBX" serve --idle 5s --refresh 2s >/dev/null 2>&1 & D=$!
sleep 3
for i in $(seq 1 "$N"); do "$SBX" create "$TAG-$i" --template postgres >/dev/null || { echo "create $i failed"; exit 1; }; done
for _ in $(seq 1 120); do
  up=$(docker ps --format '{{.Names}}' | grep -c "^sbx-$TAG-" || true)
  [ "$up" = 0 ] && break; sleep 1
done
sleep 5
up=$(docker ps --format '{{.Names}}' | grep -c "^sbx-$TAG-" || true)
rss=$(ps -o rss= -p "$D" | awk '{printf "%.1f", $1*1024/1e6}')
echo "  $up of $N containers running, daemon RSS $rss MB"
echo "result  always_on_MB=$on  sbx_MB=$rss  n=$N  settle=${SETTLE}s  image=$IMG"
