#!/usr/bin/env bash
# sbx against the field, measured on one machine under identical targets.
#
#   scripts/compare.sh [runs]            # default 10
#   CONTENDERS=sbx,lazytainer scripts/compare.sh 5
#
# The rivals are the self-hosted ones that do the same job: Sablier, Lazytainer and
# zeropod. Hosted platforms are a different category and are not measurable here.
#
# Three rules make the numbers worth publishing, and each exists because of a specific
# way a benchmark lies:
#
#   1. A sample counts only on a correct protocol reply. During development, Sablier's
#      middleware failed to engage and returned 502 in 98 ms — faster than sbx's real
#      wake. A status code is not evidence; a reply is.
#   2. A sample is VOID unless the target was verifiably asleep when the clock started.
#      A rival whose mechanism never engaged otherwise scores a spectacular wake for
#      answering while awake.
#   3. Every wake is paired with a baseline through the identical client and path, and
#      the compared quantity is the difference. The arms do not share a network path,
#      and pairing is what stops that asymmetry being credited to a contender.
#
# A contender that cannot do something by design reports N/A — that is a result.
# A contender that could not be stood up here reports SKIPPED — that is not.
set -uo pipefail

RUNS="${1:-10}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
CONTENDERS="${CONTENDERS:-sbx,sablier,lazytainer,zeropod}"
TARGETS="${TARGETS:-nginx,postgres}"
IDLE="${IDLE:-5s}"
PGCLIENT=cmp-pg-client
NET=cmp-net

# shellcheck source=lib/measure.sh
. "$ROOT/scripts/lib/measure.sh"

say()  { printf '%s\n' "$*"; }
note() { printf '  %-12s %s\n' "$1" "$2"; }

# ── results ─────────────────────────────────────────────────────────────────────
# One line per contender/target: status, then the numbers if there are any.
RESULTS=()
record() { RESULTS+=("$1|$2|$3|$4"); }   # contender, target, status, detail

# ── client preflight ────────────────────────────────────────────────────────────
# ef0993c was a client flag that existed on one platform and not another. Every client
# this script needs is checked once, by name, before anything is stood up.
preflight() {
  local missing=""
  command -v docker  >/dev/null 2>&1 || missing="$missing docker"
  command -v curl    >/dev/null 2>&1 || missing="$missing curl"
  command -v python3 >/dev/null 2>&1 || missing="$missing python3"
  if [ -n "$missing" ]; then
    say "compare: missing required tools:$missing" >&2
    exit 2
  fi
  # psql is deliberately not required on the host: it is absent on plenty of machines,
  # including the author's. It runs in a container so that "re-runnable by anyone"
  # means anyone with docker.
  docker rm -f "$PGCLIENT" >/dev/null 2>&1
  # --add-host=host-gateway is the portable form. `host.docker.internal` resolves for
  # free on Docker Desktop and colima and does NOT exist on native Linux docker, so
  # relying on it would have failed in exactly the Linux CI where the zeropod probe has
  # to run. Docker 20.10+ honours host-gateway on every platform.
  docker run -d --name "$PGCLIENT" \
    --add-host=host.docker.internal:host-gateway \
    --entrypoint sleep postgres:16-alpine 86400 >/dev/null 2>&1 \
    || { say "compare: could not start the postgres client container" >&2; exit 2; }
}

# ── validated clients ───────────────────────────────────────────────────────────
# Each returns 0 only on a correct protocol reply, never on a mere connection.
client_nginx() { # port
  local body
  body=$(curl -sf --max-time 90 "http://127.0.0.1:$1/" 2>/dev/null) || return 1
  [ -n "$body" ] || return 1
}

client_postgres() { # port
  docker exec -e PGPASSWORD=app "$PGCLIENT" \
    psql -h host.docker.internal -p "$1" -U app -d app -tAc 'select 1' 2>/dev/null \
    | grep -q '^1$'
}

# ── steady-state overhead ───────────────────────────────────────────────────────
# One connection, N requests, so the number is the per-request tax and not N process
# starts. A curl invocation per request has a millisecond floor; the tax being measured
# is ~15 us (internal/daemon/proxy_bench_test.go), which is three orders of magnitude
# below it — that measurement would print "about zero" no matter what the proxy did.
#
# The floor is measured, not assumed: the same client against a directly published
# nginx. Anything that does not clear the floor is reported as below resolution rather
# than as a number, because a delta smaller than the instrument is not a measurement.
keepalive_us_per_req() { # url, count -> median microseconds per request, or n/a
  local url="$1" count="$2" i
  local args=(-s -w '%{time_total}\n')
  # -o binds to ONE url positionally. A single -o with N urls sends the first body to
  # /dev/null and the remaining N-1 to stdout, where they land in the numbers being
  # parsed — which is exactly how this first reported "could not convert '<!DOCTYPE html>'".
  for i in $(seq 1 "$count"); do args+=(-o /dev/null "$url"); done
  # Connection reuse comes from curl's default handling of sequential same-host transfers
  # in one invocation, not from any flag — worth stating so nobody "cleans up" a flag
  # believing it is what holds the connection open.
  curl "${args[@]}" 2>/dev/null \
    | python3 -c '
import sys, statistics
xs = [float(l) for l in sys.stdin if l.strip()]
print("n/a" if not xs else f"{statistics.median(xs) * 1e6:.0f}")
'
}

NOISE_FLOOR_US=""
NOISE_JITTER_US=""
measure_noise_floor() {
  docker rm -f cmp-floor >/dev/null 2>&1
  docker run -d --name cmp-floor -p 18090:80 nginx:alpine >/dev/null 2>&1 || return 1
  local i a b
  for i in $(seq 1 30); do client_nginx 18090 && break; sleep 1; done
  # Direct vs direct: the same client against the same directly published target, twice.
  # One absolute latency is a baseline, not a floor — the floor is how much this apparatus
  # disagrees with itself, and only a delta larger than that is a measurement.
  a=$(keepalive_us_per_req "http://127.0.0.1:18090/" 50)
  b=$(keepalive_us_per_req "http://127.0.0.1:18090/" 50)
  docker rm -f cmp-floor >/dev/null 2>&1
  [ "$a" = "n/a" ] || [ "$b" = "n/a" ] && { NOISE_FLOOR_US=""; return 1; }
  NOISE_FLOOR_US="$a"
  NOISE_JITTER_US=$(python3 -c "print(abs($a - $b))")
}

# wake_sample times a wake and reports whether the first attempt was served.
#
# Measured, 2026-08-15: Lazytainer refuses connections until its packet threshold is
# crossed — attempts 1 to 5 were refused in about a millisecond each and the sixth was
# served, 5150ms after the first. It never holds a connection. sbx holds it and answers
# once the service is up, so an unmodified client sees one slow request instead of five
# failures.
#
# Reporting only latency would hide that difference entirely, and it is the more important
# fact: a client that does not retry works against one of these and not the other.
#
# Prints: "<ms> <first-ok:1|0>", or nothing when it never served.
wake_sample() { # client-fn, port, seconds
  local client="$1" port="$2" limit="$3" t0 t1 first=1 waited=0
  t0=$(measure_ms)

  while :; do
    if "$client" "$port"; then
      t1=$(measure_ms)
      printf '%s %s\n' "$((t1 - t0))" "$first"

      return 0
    fi

    first=0
    waited=$((waited + 1))

    [ "$waited" -ge "$limit" ] && return 1

    sleep 1
  done
}

# ── docker helpers shared by the container-hosted arms ──────────────────────────
container_stopped() { # name — stopped OR paused counts as asleep
  local st
  st=$(docker inspect -f '{{.State.Running}}/{{.State.Paused}}' "$1" 2>/dev/null)
  case "$st" in
    ""|"false/"*) return 0 ;;   # gone or stopped
    */true)       return 0 ;;   # paused: still "Running", and definitely not serving
    *)            return 1 ;;
  esac
}

wait_stopped() { # name, seconds
  local waited=0
  until container_stopped "$1"; do
    [ "$waited" -ge "$2" ] && return 1
    sleep 1; waited=$((waited + 1))
  done
}

image_for()  { [ "$1" = nginx ] && echo nginx:alpine || echo postgres:16-alpine; }
client_for() { [ "$1" = nginx ] && echo client_nginx || echo client_postgres; }

# ════════════════════════════════════════════════════════════════════════════════
# adapters: available · up · asleep · wake · rss · down
# ════════════════════════════════════════════════════════════════════════════════

# ── sbx ─────────────────────────────────────────────────────────────────────────
SBX_NAME=""
sbx_available() {
  [ -x "$SBX" ] || { REASON="no binary at $SBX — go build -o sbx ."; return 1; }
}
sbx_up() { # target
  SBX_NAME="cmp-$1-$$"
  "$SBX" create "$SBX_NAME" --spec "$ROOT/examples/$1/sandbox.json" >/dev/null 2>&1 || return 1
  WEB_PORT=""; DATABASE_PORT=""
  eval "$("$SBX" env "$SBX_NAME" --spec "$ROOT/examples/$1/sandbox.json" 2>/dev/null)"
  PORT=$([ "$1" = nginx ] && echo "${WEB_PORT:-}" || echo "${DATABASE_PORT:-}")
  # Unset would abort the whole run under `set -u`. Every other failure here degrades to
  # one SKIPPED row, and this one should too.
  [ -n "$PORT" ] || { REASON="sbx env exported no port for $1"; return 1; }
  "$SBX" serve --idle "$IDLE" --refresh 2s >/dev/null 2>&1 &
  SBX_DAEMON=$!
  sleep 3
}
sbx_asleep() { wait_stopped "sbx-${SBX_NAME}-$1" 60; }
# docker_provider.go:9 — client -> 20002 (wake, sbx serve listens) -> 30002 (backing,
# docker publishes). The backing port reaches the same container without the splice, so
# through-minus-backing is the proxy's cost and nothing else's.
sbx_floor_port() { echo $((PORT - 20000 + 30000)); }
sbx_rss() {
  # The daemon is a host process, not a container: docker stats cannot see it.
  local pid rss
  pid=$(pgrep -f "sbx serve" | head -1)
  [ -n "$pid" ] && rss=$(ps -o rss= -p "$pid" 2>/dev/null | tr -d ' ')
  [ -n "${rss:-}" ] && echo "$rss (ps, host process)" || echo "n/a"
}
sbx_down() {
  [ -n "${SBX_DAEMON:-}" ] && kill "$SBX_DAEMON" 2>/dev/null
  [ -n "$SBX_NAME" ] && "$SBX" rm "$SBX_NAME" >/dev/null 2>&1
  SBX_DAEMON=""
}

# ── sablier ─────────────────────────────────────────────────────────────────────
# HTTP only, by design: the wake is a Traefik middleware hook on an HTTP request, so
# nothing in it can wake a postgres client. That is N/A, not a failure.
sablier_available() { # target
  [ "$1" = postgres ] && { REASON="HTTP only — a middleware on an HTTP request cannot wake a postgres client"; return 2; }
  docker image inspect sablierapp/sablier:1.8.1 >/dev/null 2>&1 || \
    docker pull -q sablierapp/sablier:1.8.1 >/dev/null 2>&1 || { REASON="image unavailable"; return 1; }
}
sablier_up() {
  docker network create "$NET" >/dev/null 2>&1
  docker run -d --name cmp-sablier-web --network "$NET" \
    -l sablier.enable=true -l sablier.group=cmp nginx:alpine >/dev/null 2>&1 || return 1
  docker run -d --name cmp-sablier --network "$NET" \
    -v /var/run/docker.sock:/var/run/docker.sock \
    sablierapp/sablier:1.8.1 start --provider.name=docker >/dev/null 2>&1 || return 1
  sleep 4
  cat > /tmp/cmp-sablier.yml <<YAML
http:
  routers:
    web: { rule: "PathPrefix(\`/\`)", service: web, middlewares: [wake] }
  services:
    web: { loadBalancer: { servers: [ { url: "http://cmp-sablier-web:80" } ] } }
  middlewares:
    wake:
      plugin:
        sablier:
          sablierUrl: "http://cmp-sablier:10000"
          names: cmp-sablier-web
          sessionDuration: 1m
          blocking: { timeout: 60s }
YAML
  mkdir -p "$HOME/.cmp-sablier" && cp /tmp/cmp-sablier.yml "$HOME/.cmp-sablier/dynamic.yml"
  docker run -d --name cmp-sablier-traefik --network "$NET" -p 18080:80 \
    -v "$HOME/.cmp-sablier:/etc/traefik/dynamic" traefik:v3.3 \
    --experimental.plugins.sablier.moduleName=github.com/sablierapp/sablier \
    --experimental.plugins.sablier.version=v1.8.1 \
    --providers.file.directory=/etc/traefik/dynamic \
    --entrypoints.web.address=:80 >/dev/null 2>&1 || return 1
  PORT=18080
  # The middleware must demonstrably block before any number is taken from it.
  local i
  for i in $(seq 1 30); do client_nginx "$PORT" && break; sleep 1; done
  client_nginx "$PORT" || return 1
  docker stop cmp-sablier-web >/dev/null 2>&1
  if client_nginx "$PORT"; then
    return 0                      # blocked and woke it: the mechanism engaged
  fi
  REASON="middleware did not block: a request to a stopped target failed instead of waiting"
  return 1
}
sablier_asleep() { wait_stopped cmp-sablier-web 60; }
sablier_rss() {
  local a b
  a=$(docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' 2>/dev/null | measure_rss_kib cmp-sablier)
  b=$(docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' 2>/dev/null | measure_rss_kib cmp-sablier-traefik)
  echo "$a + $b (docker stats: sablier + traefik)"
}
sablier_down() {
  docker rm -f cmp-sablier-traefik cmp-sablier cmp-sablier-web >/dev/null 2>&1
  docker network rm "$NET" >/dev/null 2>&1
  rm -rf "$HOME/.cmp-sablier"
}

# ── lazytainer ──────────────────────────────────────────────────────────────────
lazytainer_available() {
  docker image inspect ghcr.io/vmorganp/lazytainer:master >/dev/null 2>&1 || \
    docker pull -q ghcr.io/vmorganp/lazytainer:master >/dev/null 2>&1 || { REASON="image unavailable"; return 1; }
}
lazytainer_up() { # target
  local img port
  img=$(image_for "$1")
  port=$([ "$1" = nginx ] && echo 80 || echo 5432)

  # Configured by LABELS on the lazytainer container, not environment variables. The env
  # form was invented here and silently discovered no group at all, so the target never
  # slept and every sample voided: its logs said only "Starting Lazytainer".
  # Source: github.com/vmorganp/Lazytainer README, read 2026-08-15.
  #   lazytainer.group.<name>.<property>   on lazytainer
  #   lazytainer.group=<name>              on the monitored container
  # Defaults there: inactiveTimeout 30s, minPacketThreshold 30, pollRate 30s, sleepMethod stop.
  docker run -d --name cmp-lazy --cap-add NET_ADMIN -p 18081:"$port" \
    -v /var/run/docker.sock:/var/run/docker.sock:ro \
    -l "lazytainer.group.cmp.ports=$port" \
    -l "lazytainer.group.cmp.inactiveTimeout=10" \
    -l "lazytainer.group.cmp.minPacketThreshold=5" \
    -l "lazytainer.group.cmp.pollRate=3" \
    -e VERBOSE=true \
    ghcr.io/vmorganp/lazytainer:master >/dev/null 2>&1 || return 1

  local env_args=()
  [ "$1" = postgres ] && env_args=(-e POSTGRES_USER=app -e POSTGRES_PASSWORD=app -e POSTGRES_DB=app)
  # ${a[@]+"${a[@]}"} — bash 3.2, which macOS still ships, calls an empty array unbound
  # under `set -u`. This expands to nothing when empty instead of aborting the adapter.
  docker run -d --name cmp-lazy-target --network "container:cmp-lazy" \
    -l lazytainer.group=cmp ${env_args[@]+"${env_args[@]}"} "$img" >/dev/null 2>&1 || return 1

  PORT=18081
  sleep 6

  local i
  for i in $(seq 1 30); do $(client_for "$1") "$PORT" && break; sleep 1; done
  $(client_for "$1") "$PORT" || { REASON="target never served through lazytainer"; return 1; }

  # Precondition: it must put the target to sleep, or there is nothing to measure.
  if ! wait_stopped cmp-lazy-target 75; then
    REASON="target never slept in 75s — check its labels against the Lazytainer README"
    return 1
  fi
}

lazytainer_asleep() { wait_stopped cmp-lazy-target 90; }
lazytainer_rss() {
  local a
  a=$(docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' 2>/dev/null | measure_rss_kib cmp-lazy)
  echo "$a (docker stats: lazytainer)"
}
lazytainer_down() { docker rm -f cmp-lazy-target cmp-lazy >/dev/null 2>&1; }

# ── zeropod ─────────────────────────────────────────────────────────────────────
# A containerd shim with CRIU, on Kubernetes. It does not stop the container: it
# checkpoints while the pod stays Running, so neither `docker inspect` nor
# `kubectl get pod` expresses "asleep". Until that observable is identified this arm
# produces no table at all — not a SKIPPED row that reads as a bad day, and certainly
# not a number taken without the gate.
zeropod_available() {
  # The observable problem comes first because it is unconditional: even with a healthy
  # cluster we would have no way to tell asleep from awake, so "no cluster" would be a
  # misleading reason to print. Fix the gate before bothering with the infrastructure.
  REASON="no verified 'checkpointed' observable: zeropod CRIU-checkpoints while the pod stays phase Running, so neither docker inspect nor kubectl get pod can express asleep. No gate means no table — scripts/zeropod-probe.sh gates on the zeropod_running metric instead"
  return 3
}
zeropod_up()     { return 1; }
zeropod_asleep() { return 1; }
zeropod_rss()    { echo "n/a"; }
zeropod_down()   { :; }

# ════════════════════════════════════════════════════════════════════════════════
# the run
# ════════════════════════════════════════════════════════════════════════════════
cleanup() {
  sbx_down; sablier_down; lazytainer_down; zeropod_down
  docker rm -f "$PGCLIENT" cmp-floor >/dev/null 2>&1
  rm -f /tmp/cmp-sablier.yml
}
trap cleanup EXIT INT TERM

measure_one() { # contender, target
  local c="$1" t="$2" client port
  client=$(client_for "$t")

  # Called directly, never inside $(...): a command substitution is a subshell, and an
  # adapter that sets PORT in a subshell sets it nowhere. That bug reported sbx itself as
  # "failed" while bench.sh was measuring it fine on the same machine.
  REASON=""
  "${c}_available" "$t"; local rc=$?
  # rc=3 — the contender cannot be gated at all, so it gets no row. A SKIPPED row reads
  # as "a bad day"; the true fact is that nothing distinguishes its asleep from its awake,
  # and a reader comparing against it deserves that stated, not a dash in a table.
  if [ "$rc" = "3" ]; then
    say "  OMITTED — $REASON"
    return
  fi
  if [ "$rc" = "2" ]; then record "$c" "$t" "N/A" "$REASON"; return; fi
  if [ "$rc" != "0" ]; then record "$c" "$t" "SKIPPED" "${REASON:-unavailable}"; return; fi

  PORT=""
  if ! "${c}_up" "$t"; then
    record "$c" "$t" "SKIPPED" "${REASON:-could not stand it up}"
    "${c}_down"; return
  fi
  port="$PORT"

  local samples="" pairs="" void=0 failed=0 transparent=0 i t0 t1 wake base
  for i in $(seq 1 "$RUNS"); do
    if ! "${c}_asleep" "$t"; then void=$((void + 1)); continue; fi

    local sample
    if ! sample=$(wake_sample "$client" "$port" 60); then
      failed=$((failed + 1))
      continue
    fi

    wake=${sample%% *}
    local firstok=${sample##* }
    [ "$firstok" = "1" ] && transparent=$((transparent + 1))

    # Paired baseline: same client, same path, target now awake.
    t0=$(measure_ms); "$client" "$port" >/dev/null 2>&1; t1=$(measure_ms)
    base=$((t1 - t0))

    samples="$samples $wake"
    pairs="$pairs $wake:$base"
    printf '    run %-3s wake %6s ms   baseline %5s ms   first-attempt %s\n' \
      "$i" "$wake" "$base" "$([ "$firstok" = 1 ] && echo served || echo REFUSED)"
  done

  local n
  n=$(printf '%s\n' $samples | measure_stat n)
  if [ "$n" = "n/a" ] || [ "$n" = "0" ]; then
    record "$c" "$t" "SKIPPED" "no valid samples (void $void, failed $failed)"
  else
    local med sd spread deltas dmed dsd dspread rss
    med=$(printf '%s\n' $samples | measure_stat median)
    sd=$(printf '%s\n' $samples | measure_stat stdev)
    deltas=$(printf '%s\n' $pairs | measure_pairs)
    dmed=$(echo "$deltas" | measure_stat median)
    dsd=$(echo "$deltas" | measure_stat stdev)
    # Below n=10 a nearest-rank p90 is just the 4th-highest sample wearing a
    # percentile's name. BENCHMARKS.md:21 already reports min/max for its n=5 row;
    # this follows that rather than inventing a second convention.
    if [ "$n" -lt 10 ]; then
      spread="min=$(printf '%s\n' $samples | measure_stat min)ms max=$(printf '%s\n' $samples | measure_stat max)ms"
      dspread="dmin=$(echo "$deltas" | measure_stat min)ms dmax=$(echo "$deltas" | measure_stat max)ms"
    else
      spread="p90=$(printf '%s\n' $samples | measure_stat p90)ms"
      dspread="dp90=$(echo "$deltas" | measure_stat p90)ms"
    fi
    rss=$("${c}_rss")
    local ovh="n/a"
    if [ "$t" = nginx ]; then
      local fa fb floor fport=""
      # Prefer the contender's own direct path: same container, same host, same client,
      # without its wake mechanism. Measuring against a separately published nginx would
      # fold two different containers into the delta and call the difference "overhead".
      declare -f "${c}_floor_port" >/dev/null 2>&1 && fport=$("${c}_floor_port")
      if [ -n "$fport" ] && client_nginx "$fport"; then
        fa=$(keepalive_us_per_req "http://127.0.0.1:$fport/" 50)
        fb=$(keepalive_us_per_req "http://127.0.0.1:$fport/" 50)
        [ "$fa" != "n/a" ] && [ "$fb" != "n/a" ] && floor="$fa"
      fi
      if [ -n "$fport" ] && [ -n "${floor:-}" ] && [ "$floor" != "n/a" ]; then
        # Interleaved, not sequential. Measuring all of the floor and then all of the
        # through path lets load drift between the two blocks land entirely in the delta:
        # the floor moved 660us -> 4280us between two runs on this machine, six times the
        # figure being measured. Alternating makes drift common-mode, which is the same
        # reason every wake carries its own paired baseline.
        # Distinct names: `pairs`, `dmed` and `dsd` already hold the WAKE pairing in this
        # same function, and reusing them here overwrote the published wake delta with the
        # overhead delta — a corrupted number that still looked plausible in the table.
        local opairs="" tu fu
        for _ in 1 2 3 4 5 6; do
          tu=$(keepalive_us_per_req "http://127.0.0.1:$port/" 20)
          fu=$(keepalive_us_per_req "http://127.0.0.1:$fport/" 20)
          [ "$tu" != "n/a" ] && [ "$fu" != "n/a" ] && opairs="$opairs $tu:$fu"
        done
        local omed osd
        omed=$(printf '%s\n' $opairs | measure_pairs | measure_stat median)
        osd=$(printf '%s\n' $opairs | measure_pairs | measure_stat stdev)
        if [ "$omed" != "n/a" ]; then
          ovh=$(measure_overhead_verdict $((floor + omed)) "$floor" "${osd:-0}")
          ovh="$ovh [interleaved, same container without the splice, n=6 pairs]"
        fi
      else
        # No same-container baseline for this contender, so the only floor available is a
        # separately published nginx — a different container, on a different bridge. The
        # delta would fold those differences in and call the result "overhead"; one such
        # measurement came out at -852us/req, faster than direct, which is not a proxy tax
        # but an artifact of comparing two containers. Refused rather than published.
        ovh="n/a — no same-container baseline; see BENCHMARKS.md"
      fi
    fi
    record "$c" "$t" "OK" "n=$n median=${med}ms $spread stdev=${sd}ms | delta median=${dmed}ms $dspread stdev=${dsd}ms | void=$void failed=$failed transparent=$transparent/$n rss=$rss overhead=$ovh"
  fi
  "${c}_down"
}

# WINDOW_CHECK=1 runs the sbx arm at two very different idle windows and prints both
# medians. If wake depends on how long the target slept, every cross-contender number is
# biased by an experimental parameter that differs per arm — so this is measured rather
# than assumed.
# The endpoints are the rivals' actual configured windows, not round numbers: lazytainer
# runs at inactiveTimeout=10s and sablier at sessionDuration=1m. Testing at anything else
# would not establish that the cross-contender table is unbiased by which window each arm
# happened to use.
LAZY_WINDOW=10s
SABLIER_WINDOW=60s
window_independence() {
  local short="$LAZY_WINDOW" long="$SABLIER_WINDOW" a b
  say "── window independence (sbx · nginx) ───────────────────────"
  IDLE="$short"; RESULTS=(); measure_one sbx nginx >/dev/null 2>&1
  a=$(detail_field "median")
  IDLE="$long";  RESULTS=(); measure_one sbx nginx >/dev/null 2>&1
  b=$(detail_field "median")
  printf '  idle %-5s median %s\n' "$short" "${a:-n/a}"
  printf '  idle %-5s median %s\n' "$long"  "${b:-n/a}"
  say "  If these differ materially, wake is not window-independent and every"
  say "  cross-contender comparison inherits that bias — bench.sh:64 assumes it does not."
  say
}

detail_field() { # key -> value from the first recorded result
  local d
  IFS='|' read -r _ _ _ d <<< "${RESULTS[0]:-|||}"
  echo "$d" | tr ' ' '\n' | grep "^$1=" | cut -d= -f2
}

main() {
  preflight
  measure_noise_floor
  measure_conditions "$SBX" extended
  note "idle window" "$IDLE (sbx) — each arm's own window is its adapter's"
  note "runs" "$RUNS per contender per target"
  note "noise floor" "${NOISE_FLOOR_US:-n/a} us/req ±${NOISE_JITTER_US:-?} — same client, nginx published directly, measured twice"
  note "idle windows" "sbx $IDLE · sablier $SABLIER_WINDOW · lazytainer $LAZY_WINDOW"
  say

  [ "${WINDOW_CHECK:-0}" = "1" ] && window_independence

  for c in ${CONTENDERS//,/ }; do
    for t in ${TARGETS//,/ }; do
      say "── $c · $t ──────────────────────────────────────────────"
      measure_one "$c" "$t"
      say
    done
  done

  say "── results ─────────────────────────────────────────────────"
  printf '  %-12s %-9s %-8s %s\n' CONTENDER TARGET STATUS DETAIL
  # ${a[@]+...} again: bash 3.2 calls an empty array unbound under `set -u`, and a run where
  # every contender was omitted is exactly when this loop is empty.
  for r in ${RESULTS[@]+"${RESULTS[@]}"}; do
    IFS='|' read -r c t s d <<< "$r"
    printf '  %-12s %-9s %-8s %s\n' "$c" "$t" "$s" "$d"
  done
  say
  say "N/A = cannot do this by design, which is a result."
  say "SKIPPED = could not be stood up here, which is not."
}

# Sourced by scripts/compare_test.sh, which replaces the adapters with stubs to prove
# that a failing one can never become a number. Only run when executed.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  main
fi
