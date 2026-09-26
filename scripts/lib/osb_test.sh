#!/usr/bin/env bash
# Tests for the process match in scripts/lib/osb.sh.
#
#   bash scripts/lib/osb_test.sh
#
# osb_api_daemons decides whether an engine is "busy with another API daemon", and the use-case
# harness waits up to 900 s while it says yes. It used to say yes about itself: its awk matched
# `/ serve / && /--osb-addr/` against every command line, and awk's own command line is that
# pattern. The fixture below is procps-shaped `ps -axo pid=,command=` output from a Linux box,
# with the lines that fooled it; the live check runs the real pipeline on this machine.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=osb.sh
. "$HERE/osb.sh"

PASS=0; FAIL=0

ok()  { PASS=$((PASS + 1)); printf '  ok   %s\n' "$1"; }
bad() { FAIL=$((FAIL + 1)); printf '  FAIL %s\n       %s\n' "$1" "$2"; }

eq() { # name, want, got
  [ "$2" = "$3" ] && ok "$1" || bad "$1" "want [$2] got [$3]"
}

echo
echo "osb.sh: osb_api_daemons"
echo "======================="

# procps right-aligns the pid and prints the full argv joined by spaces.
# shellcheck disable=SC2016 # the $1 in these lines is awk text, captured as ps shows it
fixture='      1 /sbin/init
   4242 /tmp/sbx-osb.Ab12Cd/sbx serve --osb-addr 127.0.0.1:0 --osb-host-paths /tmp/v --idle 5m --only osb
   4300 awk -v me=4242 / serve / && /--osb-addr/ && $1 != me {print $1}
   4301 awk -v me=4242 $1 != me && $3 == "serve" { for (i = 4; i <= NF; i++) { if ($i == "--osb-addr" || index($i, "--osb-addr=") == 1) { print $1; next } } }
   4400 bash -c ps -axo pid=,command= | awk '"'"'/ serve / && /--osb-addr/'"'"'
   4500 /usr/bin/bash scripts/osb-usecases-e2e.sh
   5000 /usr/local/bin/sbx serve --osb-addr=:9090 --idle 5m
   5100 /usr/local/bin/sbx serve --idle 5m
   5200 vim notes-on-serve--osb-addr.txt
   5300 /usr/local/bin/sbx exec box -- sh -c echo serve --osb-addr'

eq "a foreign API daemon is found, both --osb-addr spellings" \
  "4242 5000" "$(printf '%s\n' "$fixture" | osb_api_daemons_in | tr '\n' ' ' | sed 's/ $//')"

eq "the run's own daemon is excluded" \
  "5000" "$(printf '%s\n' "$fixture" | osb_api_daemons_in 4242 | tr '\n' ' ' | sed 's/ $//')"

eq "nothing but ourselves and the old awk is not busy" \
  "" "$(printf '%s\n' "$fixture" | grep -Ev '^ *(5000|5300) ' | osb_api_daemons_in 4242 | tr '\n' ' ' | sed 's/ $//')"

# The regression itself, kept so the fixture is known to contain what fooled the old match: the
# old awk reports its own line (4300) and the shell quoting it (4400) as foreign daemons.
old="$(printf '%s\n' "$fixture" | awk -v me=4242 '/ serve / && /--osb-addr/ && $1 != me {print $1}' | tr '\n' ' ')"
case "$old" in
  *4300*4400*) ok "the fixture reproduces the old self-match (old match said: $old)" ;;
  *) bad "the fixture reproduces the old self-match" "old match said [$old]" ;;
esac

# Live: whatever this machine is running, no pid reported may be an awk or a shell - those are
# the pipeline itself. Five runs, since whether ps sees awk depends on scheduling.
live_bad=""
for _ in 1 2 3 4 5; do
  for pid in $(osb_api_daemons); do
    comm="$(ps -o comm= -p "$pid" 2>/dev/null)"
    case "${comm##*/}" in awk|gawk|mawk|nawk|bash|sh|zsh|dash|ps) live_bad="$live_bad $pid($comm)" ;; esac
  done
done
eq "live: the pipeline never reports itself" "" "$live_bad"

echo
printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" = 0 ]
