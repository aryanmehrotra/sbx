#!/usr/bin/env bash
# Injected-sample tests for fc-jail-watch.sh's verdict.
#
#   bash scripts/fc-jail-watch_test.sh
#
# The watcher's `check` is the only thing that says CI's microvm tier ran every VMM jailed. Each
# case below hands it a sample file a real run could produce - one line per firecracker seen per
# sweep: `<sweep> <pid> <jail|host> <uid x4> <gid x4>` - and asserts on its exit status.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

PASS=0; FAIL=0

# verdict NAME WANT_EXIT LINES...: writes the lines as a sample and runs check on it.
verdict() {
  local name="$1" want="$2"; shift 2
  printf '%s\n' "$@" >"$TMP/s"
  bash "$HERE/fc-jail-watch.sh" check "$TMP/s" >"$TMP/out" 2>&1
  local got=$?
  if [ "$got" -eq "$want" ]; then
    PASS=$((PASS + 1)); printf '  ok   %s\n' "$name"
  else
    FAIL=$((FAIL + 1)); printf '  FAIL %s: exit %s, want %s\n' "$name" "$got" "$want"; sed 's/^/       /' "$TMP/out"
  fi
}

u() { printf '%s %s %s %s %s %s %s %s' "$1" "$1" "$1" "$1" "$1" "$1" "$1" "$1"; }

verdict "two VMMs, each jailed as its own uid"          0 "1 101 jail $(u 900001)" "1 102 jail $(u 900002)"
verdict "a uid reused by a later VMM at the same address" 0 "1 101 jail $(u 900001)" "2 201 jail $(u 900001)"
verdict "two VMMs sharing a uid at the same moment"     1 "1 101 jail $(u 900001)" "1 102 jail $(u 900001)"
verdict "a VMM that saw the host's /etc"                1 "1 101 host $(u 900001)"
verdict "a VMM running as root"                         1 "1 101 jail $(u 0)"
verdict "a VMM with mixed ids"                          1 "1 101 jail 900001 900001 900003 900001 $(u 900001 | cut -d' ' -f1-4)"
verdict "no VMM seen at all"                            1 ""

printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
