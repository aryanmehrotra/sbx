#!/usr/bin/env bash
# zeropod, measured — the rival that beats sbx on the thing sbx cannot do.
#
#   scripts/zeropod-probe.sh
#
# zeropod is the closest prior art to this project and the only contender that restores
# memory: it CRIU-checkpoints a container to disk and restores it on the first connection,
# with processes and open files intact. sbx restores a disk and starts the process cold.
#
# It has never been measured here, and the reason was honest but self-serving: its README
# calls arm64-in-a-Linux-VM-on-macOS "somewhat flaky", which is the author's laptop. That is
# an argument for measuring it somewhere else, not for leaving the only rival that wins
# unmeasured. This runs on Linux/amd64 with kind, which is what its own e2e suite uses.
#
# The blocker was the gate. zeropod does not stop the container — the pod stays phase
# Running while checkpointed — so `kubectl get pod` cannot tell asleep from awake, and a
# wake timed without that gate is a warm request wearing a wake's name. The observable is a
# metric: `zeropod_running` is 0 when checkpointed and 1 when running
# (docs/metrics.md, ctrox/zeropod, read 2026-08-15).
#
# Measured the same way compare.sh measures everything else, so the numbers can sit in one
# table: a sample counts only on a correct protocol reply, the target must be verifiably
# asleep at t0, and whether the FIRST attempt was served is recorded separately from how
# long it took — that column is what separates holding a connection from refusing it.
set -uo pipefail

CLUSTER="${CLUSTER:-zeropod-probe}"
RUNS="${1:-5}"
KEEP="${KEEP:-0}"

say()  { printf '%s\n' "$*"; }
note() { printf '  %-14s %s\n' "$1" "$2"; }

skip() { # reason
  say
  say "── result ──────────────────────────────────────────────────"
  say "  zeropod   SKIPPED — $1"
  say
  say "SKIPPED means it could not be stood up here. It is not a result about zeropod."
  exit 0
}

cleanup() {
  [ "$KEEP" = "1" ] && { say "  (cluster $CLUSTER left up)"; return; }
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
}
trap cleanup EXIT

command -v kind    >/dev/null 2>&1 || skip "kind is not installed"
command -v kubectl >/dev/null 2>&1 || skip "kubectl is not installed"

say "── conditions ──────────────────────────────────────────────"
note "host" "$(uname -sm)"
note "kind" "$(kind version 2>/dev/null | head -1)"
say

# ── the cluster ────────────────────────────────────────────────────────────────
# Worker nodes carry the label zeropod's RuntimeClass selects on. Taken from its own e2e
# config rather than invented: config/base/runtimeclass.yaml schedules on
# zeropod.ctrox.dev/node=true.
cat > /tmp/zeropod-kind.yaml <<'YAML'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    labels:
      zeropod.ctrox.dev/node: "true"
YAML

say "── cluster ─────────────────────────────────────────────────"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1

if ! kind create cluster --name "$CLUSTER" --config /tmp/zeropod-kind.yaml --wait 180s >/dev/null 2>&1; then
  skip "kind could not create a cluster"
fi
say "  cluster up"

# config/production, not config/kind. The kind overlay pins :dev images that are not
# published — it is for zeropod's own development, where images are built locally and loaded
# into the cluster — and using it here failed with
#   ghcr.io/ctrox/zeropod-installer:dev: not found
# after a ten-minute ImagePullBackOff. production pins released tags (v0.12.1) and the CRIU
# image the installer needs. The kind-ness of this cluster is in the node label, not the
# overlay.
#
# The installer restarts containerd on each targeted node, which is why this is a throwaway
# cluster and not anybody's dev environment.
if ! kubectl apply -k https://github.com/ctrox/zeropod/config/production >/dev/null 2>&1; then
  skip "zeropod manifests would not apply"
fi

# Longer than it looks like it needs: the init container installs a containerd shim onto
# the node and restarts containerd, which on a cold runner is image pulls plus a runtime
# restart under the very kubelet doing the watching.
if ! kubectl -n zeropod-system rollout status daemonset/zeropod-node --timeout=600s >/dev/null 2>&1; then
  # A skip that says only "it did not become ready" cannot be acted on. Everything below is
  # what someone would type next anyway, so the log has it without a second run.
  say
  say "  ── why it did not become ready ───────────────────────────"
  kubectl -n zeropod-system get pods -o wide 2>&1 | sed 's/^/    /' | head -8
  say
  kubectl -n zeropod-system describe daemonset zeropod-node 2>&1 \
    | grep -A 12 -i 'events\|conditions' | sed 's/^/    /' | head -20
  say
  for c in installer zeropod-node; do
    say "    ── logs: $c"
    kubectl -n zeropod-system logs daemonset/zeropod-node -c "$c" --tail=25 2>&1 \
      | sed 's/^/      /' | head -25
  done
  say
  kubectl get events -n zeropod-system --sort-by=.lastTimestamp 2>&1 | tail -12 | sed 's/^/    /'

  skip "the zeropod node daemonset never became ready — diagnosis above"
fi
say "  zeropod installed"

# ── the target ─────────────────────────────────────────────────────────────────
kubectl apply -f - >/dev/null 2>&1 <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: { name: probe-nginx }
spec:
  replicas: 1
  selector: { matchLabels: { app: probe-nginx } }
  template:
    metadata:
      labels: { app: probe-nginx }
      annotations:
        zeropod.ctrox.dev/scaledown-duration: 10s
    spec:
      runtimeClassName: zeropod
      containers:
        - name: nginx
          image: nginx:alpine
          ports: [{ containerPort: 80 }]
---
apiVersion: v1
kind: Service
metadata: { name: probe-nginx }
spec:
  selector: { app: probe-nginx }
  ports: [{ port: 80, targetPort: 80 }]
---
apiVersion: v1
kind: Pod
metadata: { name: probe-client }
spec:
  containers:
    - name: curl
      image: curlimages/curl:latest
      command: ["sleep", "3600"]
YAML

kubectl rollout status deployment/probe-nginx --timeout=180s >/dev/null 2>&1 \
  || skip "the target never became ready under the zeropod runtime"
kubectl wait --for=condition=ready pod/probe-client --timeout=120s >/dev/null 2>&1 \
  || skip "the client pod never became ready"
say "  target and client up"
say

# ── the gate ───────────────────────────────────────────────────────────────────
# zeropod_running is 0 when checkpointed. Without this the pod reads Running throughout and
# every "wake" would be a warm request.
# Scraped from the client pod rather than by exec'ing into zeropod's own container: that
# container is a Go image with no shell tools, so `kubectl exec ... wget` fails for a reason
# that has nothing to do with whether the metric exists. The client pod already has curl and
# is already in the cluster, which is also how a real scraper would reach it.
metrics() {
  local ip
  ip=$(kubectl -n zeropod-system get pod -l app.kubernetes.io/name=zeropod-node \
    -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)
  [ -n "$ip" ] || return 1
  kubectl exec probe-client -- curl -s -m 10 "http://$ip:8080/metrics" 2>/dev/null
}

checkpointed() {
  metrics | awk '/^zeropod_running\{/ && /probe-nginx/ { if ($NF == "0") found = 1 } END { exit !found }'
}

if ! metrics | grep -q '^zeropod_running'; then
  # Say what was actually found. "not exposed" could be a wrong port, a wrong path, TLS, or
  # a metric that simply is not there, and those need different fixes.
  say
  say "  ── what the metrics endpoint returned ────────────────────"
  kubectl -n zeropod-system get pods -o wide 2>&1 | sed 's/^/    /' | head -4
  say "    first 15 lines of the scrape:"
  metrics 2>&1 | head -15 | sed 's/^/      /'
  say "    zeropod_ metrics found:"
  metrics 2>&1 | grep -c '^zeropod_' | sed 's/^/      /'
  say

  skip "zeropod_running is not exposed, so asleep cannot be told from awake — and a wake timed without that gate is a warm request"
fi
say "  gate available: zeropod_running is exposed"

client() { # -> 0 only on a correct reply
  kubectl exec probe-client -- curl -sf -m 30 -o /dev/null http://probe-nginx/ 2>/dev/null
}

say
say "── wake, ${RUNS} runs ──────────────────────────────────────────"

samples=""
transparent=0
void=0

for i in $(seq 1 "$RUNS"); do
  waited=0
  until checkpointed; do
    sleep 2
    waited=$((waited + 2))
    [ "$waited" -ge 90 ] && break
  done

  if ! checkpointed; then
    void=$((void + 1))
    printf '  run %-3s VOID — never checkpointed in 90s\n' "$i"
    continue
  fi

  first=1
  t0=$(python3 -c 'import time;print(int(time.time()*1000))')

  attempts=0
  until client; do
    first=0
    attempts=$((attempts + 1))
    [ "$attempts" -ge 30 ] && break
    sleep 1
  done

  t1=$(python3 -c 'import time;print(int(time.time()*1000))')

  if ! client; then
    printf '  run %-3s FAILED — never served\n' "$i"
    continue
  fi

  ms=$((t1 - t0))
  samples="$samples $ms"
  [ "$first" = 1 ] && transparent=$((transparent + 1))

  printf '  run %-3s wake %6s ms   first-attempt %s\n' "$i" "$ms" \
    "$([ "$first" = 1 ] && echo served || echo REFUSED)"
done

say
say "── result ──────────────────────────────────────────────────"

n=$(printf '%s\n' $samples | grep -c . 2>/dev/null || echo 0)
if [ "${n:-0}" -eq 0 ]; then
  say "  zeropod   no valid samples (void $void)"
  exit 1
fi

# Same statistics, same rules as compare.sh: below n=10, min/median/max and never a p90.
printf '%s\n' $samples | python3 -c '
import sys, statistics
xs = sorted(int(x) for x in sys.stdin.read().split())
print(f"  n              {len(xs)}")
print(f"  median         {statistics.median(xs):.0f} ms")
print(f"  min            {xs[0]} ms")
print(f"  max            {xs[-1]} ms")
if len(xs) > 1:
    print(f"  stdev          {statistics.stdev(xs):.0f} ms")
'
note "first attempt" "$transparent/$n served"
note "void" "$void"
say
say "  what comes back: RAM and processes, via CRIU — not a cold start against a warm disk."
say "  That is the axis sbx loses on, and it is why this was worth measuring."
