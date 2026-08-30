#!/usr/bin/env bash
# The commands the other suites never run.
#
#   scripts/commands-e2e.sh
#
# usecases-e2e.sh walks the documented journeys, which is the right shape for a suite and
# leaves a gap: a command nobody wrote a use case around is a command nothing runs. Ten were in
# that state - init, with, wake, ui, url, pack, history, checkpoint and resume - so a regression
# in any of them would have reached a release with every gate green.
#
# Deliberately shallow. It asks whether each command runs, answers, and refuses what it should;
# the suites that own a behaviour still own it. A shallow test that exists beats a deep one
# that does not.
#
# Output is captured and matched with a shell pattern rather than piped to `grep -q`. Under
# `set -o pipefail` a `grep -q` that matches early closes the pipe, the command it is reading
# takes SIGPIPE, and the pipeline reports 141 - a test that fails because it succeeded. That is
# not hypothetical: it is how two of these read the first time they ran.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
TAG="cmd$$"
WORK="$(mktemp -d)"

[ -x "$SBX" ] || { echo "commands-e2e: build first: go build -o sbx ." >&2; exit 1; }

pass=0; fail=0; skipped=0
ok()   { pass=$((pass + 1));       printf '  ✓ %s\n' "$1"; }
bad()  { fail=$((fail + 1));       printf '  ✗ %s\n' "$1"; [ -n "${2:-}" ] && printf '      %s\n' "$2"; }
skip() { skipped=$((skipped + 1)); printf '  - %s (%s)\n' "$1" "$2"; }
case_() { printf '\n  %s\n' "$1"; }

# says <text> <pattern> <ok-message> <bad-message>
says() {
  case "$1" in
    *"$2"*) ok "$3" ;;
    *)      bad "$4" "$(printf '%s' "$1" | head -2)" ;;
  esac
}

DAEMON=""
cleanup() {
  [ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
  for s in $("$SBX" list 2>/dev/null | awk -v t="$TAG" '$1 ~ t {print $1}' | sort -u); do
    "$SBX" rm "$s" >/dev/null 2>&1
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

"$SBX" serve --idle 10m --refresh 3s >/dev/null 2>&1 &
DAEMON=$!
sleep 2

case_ "init: writes a spec, and works piped"

spec="$("$SBX" init --template postgres 2>/dev/null)"
says "$spec" '"services"' "piped init prints a spec" "init printed no spec"
printf '%s\n' "$spec" > "$WORK/sandbox.json"
if "$SBX" validate "$WORK/sandbox.json" >/dev/null 2>&1; then
  ok "what init printed is a spec validate accepts"
else
  bad "init produced a spec sbx rejects"
fi

case_ "with: an ephemeral sandbox that removes itself"

out="$("$SBX" with "$TAG-with" --template nginx -- sh -c 'echo RAN' 2>/dev/null)"
says "$out" "RAN" "the command ran" "the command did not run"

listing="$("$SBX" list 2>/dev/null)"
case "$listing" in
  *"$TAG-with"*) bad "the ephemeral sandbox outlived the command" ;;
  *)             ok "and the sandbox was removed after it" ;;
esac

# CI gates on this: `sbx with` must exit with the COMMAND's status, not its own.
"$SBX" with "$TAG-fail" --template nginx -- sh -c 'exit 3' >/dev/null 2>&1
status=$?
[ "$status" = "3" ] \
  && ok "the command's exit status is the one returned" \
  || bad "exit status was $status, want the command's 3"

case_ "wake and ui: bring one up on demand, and look at it"

# A moment after the `with` cases removed theirs. On a VM-backed docker the host-side port
# forward can outlive its container by a few seconds, so a create right after an rm can be
# handed a slot whose ports docker has not released - the container then never binds and never
# becomes ready. TROUBLESHOOTING.md records it; this suite reproduced it every run without the
# pause, which is a property of the runtime and not of the commands under test.
sleep 5

"$SBX" create "$TAG-w" --template nginx >/dev/null 2>&1 \
  && ok "created a sandbox to work with" || bad "create failed"

"$SBX" sleep "$TAG-w" >/dev/null 2>&1 && ok "sleep put it down" || bad "sleep failed"

wakeout="$("$SBX" wake "$TAG-w" --timeout 90s 2>&1)"
if [ $? -eq 0 ]; then
  ok "wake brought it back and waited for it"
else
  bad "wake failed" "$wakeout"
fi

awake="$("$SBX" list 2>/dev/null | awk -v s="$TAG-w" '$1 == s && $3 == "awake" {print "yes"}')"
says "$awake" "yes" "and it reports awake afterwards" "still not awake after wake"

# Not a terminal here, so this is the printOnce path, which must still print the fleet.
uiout="$("$SBX" ui 2>/dev/null)"
says "$uiout" "$TAG-w" "ui without a terminal prints the fleet" "ui printed no fleet"

case_ "history: the record of what just happened"

hist="$("$SBX" history --limit 50 2>/dev/null)"
case "$hist" in
  ?*) ok "history has something in it" ;;
  *)  bad "history is empty after all of the above" ;;
esac

hjson="$("$SBX" history --limit 5 --json 2>/dev/null)"
says "$hjson" "{" "--json is machine-readable" "--json produced nothing parseable"

hev="$("$SBX" history --limit 50 --events 2>/dev/null)"
says "$hev" "$TAG-w" "the events name the sandbox they happened to" "no event for the sandbox"

case_ "pack: a build context for a one-container platform"

if "$SBX" pack --spec "$WORK/sandbox.json" --out "$WORK/pack" >/dev/null 2>&1; then
  ok "pack produced a build context"

  found="$(find "$WORK/pack" -name Dockerfile -print -quit 2>/dev/null)"
  [ -n "$found" ] && ok "with a Dockerfile in it" || bad "no Dockerfile in the pack output"

  entry="$(find "$WORK/pack" -name 'start.sh' -print -quit 2>/dev/null)"
  [ -n "$entry" ] \
    && ok "and an entrypoint that runs sbx beside the workload" \
    || bad "no start.sh in the pack output"
else
  bad "pack failed" "$("$SBX" pack --spec "$WORK/sandbox.json" --out "$WORK/pack" 2>&1 | tail -2)"
fi

case_ "url: what it refuses (no public tunnel is opened here)"

u1="$("$SBX" url "$TAG-nope" web 2>&1)"
says "$u1" "no service" "an unknown sandbox is refused by name" "an unknown sandbox was not refused"

u2="$("$SBX" url "$TAG-w" nginx --host-header nonsense 2>&1)"
says "$u2" "rewrite or pass" "an unknown --host-header value is refused" "a bad --host-header was accepted"

u3="$("$SBX" url "$TAG-w" nginx --via nosuchtunnel 2>&1)"
says "$u3" "not installed" "an unknown --via backend is refused" "an unknown --via was accepted"

case_ "checkpoint and resume: the process, not just the disk"

if [ "$(uname -s)" != "Linux" ]; then
  # The refusal IS the behaviour this platform can prove: CRIU needs a Linux host.
  msg="$("$SBX" checkpoint "$TAG-w" mid 2>&1)"
  case "$msg" in
    *[Ll]inux*|*CRIU*|*criu*) ok "checkpoint is refused with a reason off Linux" ;;
    *)                        bad "checkpoint gave no clear refusal" "$msg" ;;
  esac
  skip "checkpoint and resume end to end" "CRIU needs a Linux host"
else
  if "$SBX" checkpoint "$TAG-w" mid >/dev/null 2>&1; then
    ok "checkpoint took a dump"
    "$SBX" resume "$TAG-w" mid >/dev/null 2>&1 \
      && ok "resume restored it" \
      || bad "resume failed - see TROUBLESHOOTING on docker vs podman"
  else
    skip "checkpoint and resume" "checkpoint unavailable on this host"
  fi
fi

echo
echo "───────────────────────────────────────────────────────────"
echo "  $pass passed, $fail failed, $skipped skipped"
[ "$fail" -eq 0 ]
