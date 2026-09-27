#!/usr/bin/env bash
# OpenSandbox's own server against sbx serve: same API, same Go SDK client, same machine.
#
#   scripts/bench-osb-upstream.sh [ROUNDS] [IDLE_N]      # defaults 10 and 10
#
# Starts upstream's published server image (the tag in test/osb/UPSTREAM) on loopback with the
# docker socket, as upstream's server/docker-compose.example.yaml does, then:
#   1. scripts/osb-bench.sh --compare: create -> first command and the rest of the lifecycle,
#      both servers interleaved and rotated every round, image node:22-slim.
#   2. scripts/osb-bench.sh --burst 1 --rounds 1 on each, alternating, ROUNDS times: ComputeSDK's
#      TTI shape (create -> runCommand('node -v')). The bench's burst mode takes one target,
#      so the alternation happens here.
#   3. Memory: IDLE_N sandboxes on each server, left untouched 60 s, then the docker stats sum
#      of their containers and the server process's RSS.
# Upstream's Docker runtime has no warm pool (its poolRef is Kubernetes-only and refused on
# docker with SANDBOX::UNSUPPORTED_POOL_REF), so there is no pooled row to pair.
#
# Needs docker and Go, and an engine with no sbx sandboxes on it (see scripts/lib/osb.sh).
# Used for the OpenSandbox column of docs/BENCHMARKS.md.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/osb.sh
. "$ROOT/scripts/lib/osb.sh"
# shellcheck source=scripts/lib/measure.sh
. "$ROOT/scripts/lib/measure.sh"

ROUNDS=${1:-10}; IDLE_N=${2:-10}
IMAGE=node:22-slim
TAG="$(sed -n 's/^tag=//p' "$ROOT/test/osb/UPSTREAM" | head -1)"
UP_IMAGE="opensandbox/server:$TAG"
# execd is the agent upstream injects into each sandbox; same release as the server.
EXECD_IMAGE="opensandbox/execd:v${TAG#release-}"
UP_NAME="sbx-bench-osb-upstream-$$"
UP_KEY="bench-$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
UP_URL=""

cleanup() {
  # Upstream's sandboxes first, through its API, then its containers by name in case the
  # server died first. Only this run's server is touched.
  if [ -n "$UP_URL" ]; then
    for id in $(curl -s -m10 -H "OPEN-SANDBOX-API-KEY: $UP_KEY" "$UP_URL/v1/sandboxes?pageSize=200" |
      python3 -c 'import json,sys; [print(i["id"]) for i in json.load(sys.stdin).get("items",[])]' 2>/dev/null); do
      curl -s -m30 -X DELETE -H "OPEN-SANDBOX-API-KEY: $UP_KEY" "$UP_URL/v1/sandboxes/$id" >/dev/null
    done
  fi
  docker rm -f "$UP_NAME" >/dev/null 2>&1
  osb_teardown
}

osb_init
trap cleanup EXIT
osb_build_tools
osb_resolve_docker ""

for i in "$UP_IMAGE" "$EXECD_IMAGE" "$IMAGE"; do
  docker image inspect "$i" >/dev/null 2>&1 || docker pull -q "$i" >/dev/null || osb_die "could not pull $i"
done

# Host network, so the sandbox ports upstream publishes are reachable at 127.0.0.1 by both the
# server and the SDK. Upstream's compose file does the same thing with host.docker.internal.
port="$("$OSB_HARNESS" freeport)" || osb_die "no free port"
UP_URL="http://127.0.0.1:$port"
cat > "$OSB_WORK/upstream.toml" <<T
[server]
host = "127.0.0.1"
port = $port
api_key = "$UP_KEY"
[log]
level = "WARNING"
[runtime]
type = "docker"
execd_image = "$EXECD_IMAGE"
[docker]
network_mode = "bridge"
host_ip = "127.0.0.1"
[ingress]
mode = "direct"
T
docker run -d --name "$UP_NAME" --network host -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$OSB_WORK/upstream.toml:/etc/opensandbox/config.toml:ro" \
  -e SANDBOX_CONFIG_PATH=/etc/opensandbox/config.toml "$UP_IMAGE" >/dev/null ||
  osb_die "could not start $UP_IMAGE; check: docker logs $UP_NAME"
"$OSB_HARNESS" ready -url "$UP_URL" -key "$UP_KEY" -timeout 60s >/dev/null ||
  osb_die "upstream server never answered on $UP_URL; check: docker logs $UP_NAME"
osb_say "upstream $UP_IMAGE on $UP_URL"
measure_conditions "$ROOT/sbx"

echo "── 1. lifecycle, interleaved ($ROUNDS rounds, $IMAGE) ──"
"$ROOT/scripts/osb-bench.sh" --compare "$UP_URL" --key "$UP_KEY" --image "$IMAGE" --rounds "$ROUNDS" 2>/dev/null

echo "── 2. burst 1, alternating ($ROUNDS rounds each) ──"
# One round per call so sbx and upstream alternate; the order flips every round.
for r in $(seq 1 "$ROUNDS"); do
  if [ $((r % 2)) = 1 ]; then order="sbx up"; else order="up sbx"; fi
  for who in $order; do
    if [ "$who" = sbx ]; then
      "$ROOT/scripts/osb-bench.sh" --burst 1 --rounds 1 2>/dev/null
    else
      "$ROOT/scripts/osb-bench.sh" --external "$UP_URL" --key "$UP_KEY" --burst 1 --rounds 1 2>/dev/null
    fi | sed -n "s/^| *default *|/burst $who round $r |/p" | tee -a "$OSB_WORK/burst.txt"
  done
done
# Column 4 is the round's median, which with one sandbox is its only sample. A failed round
# prints "-" there and is not counted.
for who in sbx up; do
  ys=$(awk -F'|' -v w="burst $who " 'index($1, w) == 1 && $4 + 0 > 0 {print $4 + 0}' "$OSB_WORK/burst.txt")
  echo "burst $who n=$(echo "$ys" | measure_stat n) median=$(echo "$ys" | measure_stat median) min=$(echo "$ys" | measure_stat min) max=$(echo "$ys" | measure_stat max)"
done

echo "── 3. memory of $IDLE_N idle sandboxes after 60 s ──"
osb_start_daemon

# idle_mem NAME URL KEY PIDSPEC - create IDLE_N sandboxes, wait 60 s, print the sum of their
# containers' memory (docker stats, KiB) and the server's RSS. The containers are the ones
# that appeared during the creates; nothing else is running on the engine.
idle_mem() {
  local name="$1" url="$2" key="$3" before after ids sum rss c
  before=$(docker ps -q --no-trunc | sort)
  for c in $(seq 1 "$IDLE_N"); do
    curl -s -m120 -H "OPEN-SANDBOX-API-KEY: $key" -H 'Content-Type: application/json' \
      -d "{\"image\":{\"uri\":\"$IMAGE\"},\"entrypoint\":[\"tail\",\"-f\",\"/dev/null\"],\"timeout\":3600,\"resourceLimits\":{\"cpu\":\"1\",\"memory\":\"2Gi\"},\"metadata\":{\"bench\":\"idle\"}}" \
      "$url/v1/sandboxes" | grep -q '"id"' || echo "$name: create $c failed"
  done
  sleep 60
  after=$(docker ps -q --no-trunc | sort)
  ids=$(comm -13 <(echo "$before") <(echo "$after"))
  sum=0
  for c in $ids; do
    k=$(docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$c" | measure_rss_kib "$(docker inspect -f '{{.Name}}' "$c" | tr -d /)")
    [ "$k" = n/a ] || sum=$((sum + k))
  done
  rss=$(ps -o rss= -p "$4" | awk '{s+=$1} END {print s+0}')
  echo "idle $name: $(echo "$ids" | grep -c .) containers, $((sum / 1024)) MiB (docker stats sum); server RSS $((rss / 1024)) MiB"
  # Removed through the API so the next server starts on an empty engine.
  for c in $(curl -s -m10 -H "OPEN-SANDBOX-API-KEY: $key" "$url/v1/sandboxes?pageSize=200" |
    python3 -c 'import json,sys; [print(i["id"]) for i in json.load(sys.stdin).get("items",[])]' 2>/dev/null); do
    curl -s -m60 -X DELETE -H "OPEN-SANDBOX-API-KEY: $key" "$url/v1/sandboxes/$c" >/dev/null
  done
  sleep 5
}

# Every process in the upstream container (uvicorn and any workers), as the host sees them.
up_pids=$(docker top "$UP_NAME" -o pid | tail -n +2 | tr '\n' ',' | sed 's/,$//')
idle_mem upstream "$UP_URL" "$UP_KEY" "$up_pids"
idle_mem sbx "$OSB_URL" "$OSB_KEY" "$OSB_DAEMON"
