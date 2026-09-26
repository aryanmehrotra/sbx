#!/usr/bin/env bash
# The OpenSandbox lifecycle, timed through the upstream Go SDK.
#
#   scripts/osb-bench.sh [--rounds 5]                    # a throwaway sbx
#   scripts/osb-bench.sh --external http://host:8080     # someone else's server only
#   scripts/osb-bench.sh --compare  http://host:8080     # sbx AND that server, interleaved
#
#   scripts/osb-bench.sh --burst 100 [--pool node:22-slim=100]  # ComputeSDK Burst TTI
#
# Other flags: --key K (for --external/--compare; else OPENSANDBOX_TEST_API_KEY),
# --idle DUR (start sbx with --idle DUR and measure a wake from the idle freeze after
# waiting past it), --image IMG, --docker-host URL, --keep-logs.
# --burst N switches to ComputeSDK's Burst TTI: N concurrent create -> runCommand('node -v') per
# round, image node:22-slim unless --image, median/p95/p99/success and their score. --pool SPEC
# starts sbx with --osb-pool SPEC and waits for full pools before each round; --burst-modes
# default,cold also times creates that bypass the pool.
# --provider firecracker runs the throwaway sbx on microVMs (Linux, /dev/kvm, sudo -n; see
# osb-conformance.sh), and --pool-freeze parks its pool members frozen instead of asleep.
#
# Measures create -> first command, command round trip, 1 MiB upload and download, and
# pause -> resume -> first command; the numbers come from test/osb/bench, which interleaves
# targets every round and rotates their order (CONTRIBUTING.md: interleave and alternate).
# Prints a markdown table. It needs a daemon with the lifecycle API, and says so when the
# sbx it built has none.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/osb.sh
. "$ROOT/scripts/lib/osb.sh"

ROUNDS=5
EXTERNAL=""
COMPARE=""
KEY=""
IMAGE=""
DOCKER_URL=""
BURST=""
POOL=""
MODES=""
PROVIDER="docker"

usage() { sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    --rounds)      ROUNDS="${2:?--rounds needs a number}"; shift 2 ;;
    --external)    EXTERNAL="${2:?--external needs a URL}"; shift 2 ;;
    --compare)     COMPARE="${2:?--compare needs a URL}"; shift 2 ;;
    --key)         KEY="${2:?--key needs a value}"; shift 2 ;;
    --idle)        OSB_IDLE="${2:?--idle needs a duration}"; shift 2 ;;
    --image)       IMAGE="${2:?--image needs an image}"; shift 2 ;;
    --docker-host) DOCKER_URL="${2:?--docker-host needs a URL}"; shift 2 ;;
    --keep-logs)   OSB_KEEP_WORK=1; shift ;;
    --burst)       BURST="${2:?--burst needs a number}"; shift 2 ;;
    --pool)        POOL="${2:?--pool needs IMAGE[=N]}"; shift 2 ;;
    --burst-modes) MODES="${2:?--burst-modes needs default,cold}"; shift 2 ;;
    --provider)    PROVIDER="${2:?--provider needs docker or firecracker}"; shift 2 ;;
    --pool-freeze) OSB_POOL_FREEZE=1; shift ;;
    -h|--help)     usage; exit 0 ;;
    *)             usage >&2; osb_die "unknown argument '$1'" ;;
  esac
done

[ -n "$EXTERNAL" ] && [ -n "$COMPARE" ] && osb_die "--external runs that server alone and --compare runs it beside sbx; give one"
[ -n "$EXTERNAL" ] && [ -n "$OSB_IDLE" ] && osb_die "--idle configures the sbx this starts; with --external there is none"
case "$PROVIDER" in
  docker) ;;
  firecracker|fc) PROVIDER=firecracker
    [ -z "$EXTERNAL" ] || osb_die "--provider firecracker starts its own daemon; --external names one already running"
    [ -z "$OSB_IDLE" ] || osb_die "--idle is not supported with --provider firecracker" ;;
  *) osb_die "--provider is docker or firecracker, not '$PROVIDER'" ;;
esac
[ "$OSB_POOL_FREEZE" = 1 ] && [ "$PROVIDER" != firecracker ] && osb_die "--pool-freeze is for --provider firecracker (on docker, restart sbx serve with --osb-pool-freeze)"
[ "$OSB_POOL_FREEZE" = 1 ] && [ -z "$POOL" ] && osb_die "--pool-freeze needs --pool"

osb_init
if [ -n "$EXTERNAL" ] || [ "$PROVIDER" = firecracker ]; then osb_build_tools nosbx; else osb_build_tools; fi

BENCH="$OSB_WORK/bench"
(cd "$ROOT/test/osb" && GOWORK=off go build -o "$BENCH" ./bench) || osb_die "could not build test/osb/bench"

OTHER="${EXTERNAL:-$COMPARE}"
OTHER_KEY="${KEY:-${OPENSANDBOX_TEST_API_KEY:-}}"

[ -z "$IMAGE" ] && [ -z "$BURST" ] && IMAGE="python:3.11-slim"
[ -n "$POOL" ] && [ -n "$EXTERNAL" ] && osb_die "--pool configures the sbx this starts; with --external there is none"

# Positional parameters rather than an array: bash 3.2 and set -u (see lib/osb.sh).
set -- -rounds "$ROUNDS"
[ -n "$IMAGE" ] && set -- "$@" -image "$IMAGE"

if [ -n "$BURST" ]; then
  set -- "$@" -burst "$BURST"
  [ -n "$MODES" ] && set -- "$@" -burst-modes "$MODES"
  [ -n "$POOL" ] && set -- "$@" -wait-pool
fi

# sbx serve reads it as --osb-pool; exported so the throwaway daemon inherits it.
[ -n "$POOL" ] && export SBX_OSB_POOL="$POOL"

if [ -z "$EXTERNAL" ]; then
  if [ "$PROVIDER" = firecracker ]; then
    osb_fc_start_daemon
  else
    osb_resolve_docker "$DOCKER_URL"
    osb_start_daemon
  fi
  export OSB_KEY_SBX="$OSB_KEY"
  set -- "$@" -target "sbx=$OSB_URL"
  SBX_URL="$OSB_URL"
fi

if [ -n "$OTHER" ]; then
  # Checked with the harness's ready probe so a wrong URL fails here, clearly, and not as
  # a table of errors. osb_use_external overwrites OSB_URL/KEY, so sbx's were saved above.
  saved_url="$OSB_URL"; saved_key="$OSB_KEY"
  osb_use_external "$OTHER" "$OTHER_KEY"
  OSB_URL="$saved_url"; OSB_KEY="$saved_key"
  export OSB_KEY_OTHER="$OTHER_KEY"
  set -- "$@" -target "other=${OTHER%/}"
fi

if [ -n "$OSB_IDLE" ]; then
  set -- "$@" -server-idle "$OSB_IDLE"
fi

echo "── bench: ${SBX_URL:+sbx $SBX_URL }${OTHER:+other $OTHER} ──"
"$BENCH" "$@"
rc=$?

# On microVMs, say how many creates the pool actually served: "default" rounds that all missed
# it would be cold numbers under a pool label.
if [ "$PROVIDER" = firecracker ] && [ -n "$POOL" ]; then
  osb_say "creates answered from the warm pool: $(osb_fc_pool_hits)"
fi

exit $rc
