#!/usr/bin/env bash
# OpenSandbox's own Go e2e suite, unmodified, against sbx.
#
#   scripts/osb-conformance.sh                          # the v0.9.0 tier, against a throwaway sbx
#   scripts/osb-conformance.sh --tier v0.10.0           # a later tier (cumulative)
#   scripts/osb-conformance.sh --files sandbox,command  # just these upstream files
#   scripts/osb-conformance.sh -run 'Renew|Endpoint'    # and only tests matching this
#   scripts/osb-conformance.sh --external http://host:8080 [--key K]
#                                                       # any server: a real OpenSandbox to
#                                                       # compare, or an sbx you already run
#
# Other flags: --docker-host URL (the engine the throwaway daemon uses; must hold no sbx
# sandboxes), --no-key (start sbx without --osb-key; upstream's e2e_test.go sends none),
# --timeout DUR (go test -timeout, default 30m), --keep-logs.
#
# "Compatible" is not a feature table here. It is upstream's tests/go at the commit pinned in
# test/osb/UPSTREAM, fetched and run as-is, reported per test. A SKIP is not a PASS: any skip
# not allowed by test/osb/expectations - for that test, with that message - fails the run,
# as does a test that never reported, and a package that failed outside any test.
#
# The exit status is the gate. It comes from the harness's reading of go test's JSON
# stream AND go test's own exit status, never from a pipeline's last command.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/osb.sh
. "$ROOT/scripts/lib/osb.sh"

RUN=""
FILES=""
TIER=""
EXTERNAL=""
KEY=""
NOKEY=""
DOCKER_URL=""
TIMEOUT="30m"

usage() { sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; }

while [ $# -gt 0 ]; do
  case "$1" in
    -run|--run)    RUN="${2:?-run needs a regexp}"; shift 2 ;;
    --files)       FILES="${2:?--files needs a list}"; shift 2 ;;
    --tier)        TIER="${2:?--tier needs a release}"; shift 2 ;;
    --external)    EXTERNAL="${2:?--external needs a URL}"; shift 2 ;;
    --key)         KEY="${2:?--key needs a value}"; shift 2 ;;
    --no-key)      NOKEY="--no-key"; shift ;;
    --docker-host) DOCKER_URL="${2:?--docker-host needs a URL}"; shift 2 ;;
    --timeout)     TIMEOUT="${2:?--timeout needs a duration}"; shift 2 ;;
    --keep-logs)   OSB_KEEP_WORK=1; shift ;;
    -h|--help)     usage; exit 0 ;;
    *)             usage >&2; osb_die "unknown argument '$1'" ;;
  esac
done

[ -n "$FILES" ] && [ -n "$TIER" ] && osb_die "--files and --tier choose the same thing; give one"
[ -n "$KEY" ] && [ -z "$EXTERNAL" ] && osb_die "--key is for --external; the throwaway daemon makes its own"

osb_init
osb_fetch_upstream

if [ -n "$EXTERNAL" ]; then osb_build_tools nosbx; else osb_build_tools; fi

EXP="$ROOT/test/osb/expectations"
TESTS_DIR="$OSB_SRC/tests/go"

if [ -z "$FILES" ]; then
  FILES="$("$OSB_HARNESS" tier -expectations "$EXP" -tier "${TIER:-v0.9.0}")" || exit 1
fi

# Every test the selection names. Asking go test for exactly these, and then checking each
# one reported, is what makes "the package passed" and "these tests passed" the same claim.
"$OSB_HARNESS" tests -dir "$TESTS_DIR" -files "$FILES" -match "$RUN" > "$OSB_WORK/expected.tsv" || exit 1
RUN_RE="^($(cut -f1 "$OSB_WORK/expected.tsv" | paste -sd'|' -))\$"
osb_say "selected $(wc -l < "$OSB_WORK/expected.tsv" | tr -d ' ') tests from: $FILES"

# Compile (and download the suite's modules) before any server is involved, so a build
# problem is reported as one and does not burn the daemon's time or look like a server bug.
(cd "$TESTS_DIR" && GOWORK=off go test -count=1 -run '^$' . >"$OSB_WORK/compile.log" 2>&1) || {
  cat "$OSB_WORK/compile.log" >&2
  osb_die "the upstream suite did not compile"
}

if [ -n "$EXTERNAL" ]; then
  osb_use_external "$EXTERNAL" "${KEY:-${OPENSANDBOX_TEST_API_KEY:-}}"
else
  osb_resolve_docker "$DOCKER_URL"
  if [ -n "$NOKEY" ]; then osb_start_daemon --no-key; else osb_start_daemon; fi
fi

osb_export_suite_env

echo
echo "── upstream $(osb_pin tag) ($(osb_pin commit | cut -c1-12)) against $OSB_URL ──"

(cd "$TESTS_DIR" && GOWORK=off go test -json -count=1 -timeout "$TIMEOUT" -run "$RUN_RE" .) \
  2>"$OSB_WORK/go-test.stderr" |
  "$OSB_HARNESS" report -expectations "$EXP" -expected "$OSB_WORK/expected.tsv" \
    -save "$OSB_WORK/go-test.json"
codes=("${PIPESTATUS[@]}")
go_rc="${codes[0]}"
report_rc="${codes[1]}"

if [ -s "$OSB_WORK/go-test.stderr" ]; then
  echo
  echo "go test stderr:"
  sed 's/^/  /' "$OSB_WORK/go-test.stderr" | tail -40
fi

if [ "$report_rc" -ne 0 ]; then
  osb_say "raw stream: $OSB_WORK/go-test.json"
  exit 1
fi

# The report was green but go test was not: something the stream did not show. Never
# resolved in the report's favour.
if [ "$go_rc" -ne 0 ]; then
  osb_say "go test exited $go_rc although every selected test reported a pass or an allowed skip; raw stream: $OSB_WORK/go-test.json"
  exit 1
fi

exit 0
