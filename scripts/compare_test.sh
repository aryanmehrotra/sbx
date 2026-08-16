#!/usr/bin/env bash
# Injected-failure tests for compare.sh's gating logic.
#
#   bash scripts/compare_test.sh
#
# scripts/lib/measure_test.sh proves the maths. This proves the thing the maths cannot:
# that a contender having a bad day never becomes a number. Every test below replaces the
# adapters with stubs that fail in one specific way and asserts on the recorded status.
#
# It is deliberately hostile. The failure this harness exists to prevent is a flattering
# number, and the only way to know it prevents one is to hand it a contender that deserves
# no number and check that it gets none.
#
# SC2034: RUNS, PORT, REASON and NOISE_FLOOR_US are compare.sh's variables, set here to
# drive one branch at a time. They are consumed by the sourced script, not by this file,
# so shellcheck cannot see the use. The directive has to precede the first command to
# apply file-wide.
# shellcheck disable=SC2034
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Sourcing runs no contenders: compare.sh guards main() behind BASH_SOURCE.
# shellcheck source=compare.sh
. "$HERE/compare.sh"
# After the source, not before: compare.sh assigns RUNS="${1:-10}" as it loads, so an
# exported value would be overwritten. Two runs is enough to exercise every branch.
RUNS=2

PASS=0; FAIL=0
ok()  { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n       %s\n' "$1" "$2"; }
skipped() { printf '  skip %s (%s)\n' "$1" "$2"; }

# The status recorded for the single contender under test.
status_of() { IFS='|' read -r _ _ s _ <<< "${RESULTS[0]:-||NONE|}"; echo "$s"; }
detail_of() { IFS='|' read -r _ _ _ d <<< "${RESULTS[0]:-|||}"; echo "$d"; }

reset() { RESULTS=(); REASON=""; NOISE_FLOOR_US=""; }

# A stub contender. Each test overrides only the verb it is exercising.
stub_available() { return 0; }
stub_up()        { PORT=9999; return 0; }
stub_asleep()    { return 0; }
stub_rss()       { echo "0 (stub)"; }
stub_down()      { :; }
stub_client()    { return 0; }
client_for()     { echo stub_client; }   # every target uses the stub client

echo
echo "compare.sh - injected failures"
echo "=============================="

# 1. `up` fails → SKIPPED, and nothing is measured.
reset
stub_up() { REASON="the target never came up"; return 1; }
measure_one stub nginx >/dev/null 2>&1
[ "$(status_of)" = "SKIPPED" ] \
  && ok "up fails → SKIPPED, no number" \
  || bad "up fails" "got status [$(status_of)]"

# 2. The client never returns a correct reply → no samples, so no row of numbers.
#    This is the Sablier 502 case: fast, successful-looking, and wrong.
reset
stub_up() { PORT=9999; return 0; }
stub_client() { return 1; }
measure_one stub nginx >/dev/null 2>&1
[ "$(status_of)" = "SKIPPED" ] && [[ "$(detail_of)" == *"failed 2"* || "$(detail_of)" == *"failed=2"* || "$(detail_of)" == *"no valid samples"* ]] \
  && ok "client never replies correctly → SKIPPED, failures counted" \
  || bad "client failure" "got [$(status_of)] [$(detail_of)]"

# 3. The target is never observed asleep → every sample VOID, never recorded.
#    A contender whose mechanism did not engage must not score a wake for answering
#    while it was already awake.
reset
stub_client() { return 0; }
stub_asleep() { return 1; }
measure_one stub nginx >/dev/null 2>&1
[ "$(status_of)" = "SKIPPED" ] && [[ "$(detail_of)" == *"void"* ]] \
  && ok "never asleep → all samples VOID, none recorded" \
  || bad "void gating" "got [$(status_of)] [$(detail_of)]"

# 4. A capability boundary is a result, not a failure, and must not read as one.
reset
stub_asleep() { return 0; }
stub_available() { REASON="does not speak this protocol"; return 2; }
measure_one stub postgres >/dev/null 2>&1
[ "$(status_of)" = "N/A" ] \
  && ok "rc=2 → N/A, distinct from SKIPPED" \
  || bad "N/A vs SKIPPED" "got [$(status_of)]"

# 5. A contender that cannot be gated at all gets no row whatsoever - not even a dash.
reset
stub_available() { REASON="nothing distinguishes asleep from awake"; return 3; }
measure_one stub nginx >/dev/null 2>&1
[ "${#RESULTS[@]}" -eq 0 ] \
  && ok "rc=3 → omitted entirely, no row to misread" \
  || bad "omission" "expected no rows, got ${#RESULTS[@]}: $(status_of)"

# 6. The happy path still records numbers - a gate that rejects everything is useless.
reset
stub_available() { return 0; }
measure_one stub nginx >/dev/null 2>&1
[ "$(status_of)" = "OK" ] && [[ "$(detail_of)" == *"n=2"* ]] \
  && ok "healthy contender → OK with a distribution" \
  || bad "happy path" "got [$(status_of)] [$(detail_of)]"

# 7. Below n=10 the row must not carry a p90 - it would be the second-highest sample
#    wearing a percentile's name.
[[ "$(detail_of)" == *"min="* && "$(detail_of)" != *" p90="* ]] \
  && ok "n<10 reports min/max, never p90" \
  || bad "low-n spread" "got [$(detail_of)]"

# 8. Teardown after a kill. The adapters start real containers, networks and files, and
#    only the trap stands between an interrupted run and leaked state. `trap ... EXIT`
#    alone does not run when bash is killed by a signal it has not trapped, which is why
#    the trap lists INT and TERM.
if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  docker rm -f cmp-pg-client >/dev/null 2>&1
  ( CONTENDERS=sbx TARGETS=nginx bash "$HERE/compare.sh" 1 >/dev/null 2>&1 ) &
  runner=$!
  w=0
  while [ $w -lt 30 ]; do
    docker inspect cmp-pg-client >/dev/null 2>&1 && break
    sleep 1; w=$((w + 1))
  done
  if docker inspect cmp-pg-client >/dev/null 2>&1; then
    kill -TERM "$runner" 2>/dev/null
    w=0
    while [ $w -lt 20 ] && docker inspect cmp-pg-client >/dev/null 2>&1; do sleep 1; w=$((w + 1)); done
    docker inspect cmp-pg-client >/dev/null 2>&1 \
      && bad "teardown after a kill" "cmp-pg-client survived SIGTERM" \
      || ok "teardown after a kill leaves no containers behind"
  else
    skipped "teardown after a kill" "client container never appeared"
  fi
  wait "$runner" 2>/dev/null
  docker rm -f cmp-pg-client >/dev/null 2>&1
else
  skipped "teardown after a kill" "docker unavailable"
fi

echo
echo "----------------------------------------"
printf 'passed %d · failed %d\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
