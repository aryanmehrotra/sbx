#!/usr/bin/env bash
# `--provider firecracker` on a Mac, end to end, through the helper VM.
#
#   scripts/fc-anywhere-e2e.sh
#
# Creates a helper VM (lima, else colima; nested virtualisation, 2 CPU / 2 GiB), starts the
# host half of `sbx serve --provider firecracker`, creates a microVM sandbox from the Mac,
# wakes it with a plain TCP connect from the Mac, runs a command in it, and removes everything
# it created. The unit tests prove the commands sbx would run and the forwarding wiring against
# a fake daemon; this is the only thing that proves a real VM, a real Firecracker and a real
# wake.
#
# Then it sleeps and wakes that sandbox FC_E2E_ROUNDS times (default 10) - a Diff snapshot, a
# restore and an execd re-key each time - timing the wake to first byte, an awake request and an
# exec round trip, with a create of a second sandbox interleaved, and prints median/p95 (also
# appended to $FC_E2E_RESULTS when that is set).
#
# Heavy: a VM create is a download and about a minute, and the VM holds 2 GiB while it runs.
# It uses its own VM name (sbx-fc-e2e) so a helper VM somebody already uses is never touched,
# and it refuses to start if that name exists, so it only ever deletes what it made.
#
# Needs: Apple M3 or later, macOS 15+, limactl or colima, and the Firecracker provider on this
# branch (the in-VM daemon runs `sbx serve --provider firecracker`).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
WORK="$(mktemp -d)"
NAME="e2e-fc-$$"
ROUNDS="${FC_E2E_ROUNDS:-10}"

export SBX_FC_VM_NAME="${SBX_FC_VM_NAME:-sbx-fc-e2e}"
export SBX_PROVIDER_KIND=firecracker

[ -x "$SBX" ] || { echo "fc-anywhere-e2e: build first: go build -o sbx ." >&2; exit 1; }
[ "$(uname -s)" = Darwin ] || { echo "fc-anywhere-e2e: this is the macOS helper-VM path; on Linux with /dev/kvm the provider runs directly" >&2; exit 1; }

pass=0; fail=0
ok()  { pass=$((pass + 1)); printf '  ✓ %s\n' "$1"; }
bad() { fail=$((fail + 1)); printf '  ✗ %s\n' "$1"; [ -n "${2:-}" ] && printf '      %s\n' "$2"; }

backend=$("$SBX" fc backend | head -1)
case "$backend" in
  helper-vm*) ok "backend: $backend" ;;
  *) echo "fc-anywhere-e2e: this Mac cannot run the helper VM:" >&2; "$SBX" fc backend >&2; exit 1 ;;
esac

if ! "$SBX" fc vm status | grep -q ': absent$'; then
  echo "fc-anywhere-e2e: $SBX_FC_VM_NAME already exists; this script only deletes a VM it created." >&2
  echo "  remove it first: SBX_FC_VM_NAME=$SBX_FC_VM_NAME $SBX fc vm rm --yes" >&2
  exit 1
fi

# colima switches the global docker context on start; sbx must put it back. Checked, not trusted.
ctx_before=$(docker context show 2>/dev/null || true)

front_pid=""

cleanup() {
  [ -n "$front_pid" ] && kill "$front_pid" 2>/dev/null && wait "$front_pid" 2>/dev/null
  # Only against a running VM: a redirected command starts the helper VM on demand, which from
  # here would recreate the thing this is about to delete.
  if "$SBX" fc vm status | grep -q ': running'; then "$SBX" rm "$NAME" >/dev/null 2>&1 || true; fi
  "$SBX" fc vm rm --yes >/dev/null 2>&1 || true

  if "$SBX" fc vm status | grep -q ': absent$'; then ok "helper VM removed"; else bad "helper VM $SBX_FC_VM_NAME is still there"; fi

  ctx_after=$(docker context show 2>/dev/null || true)
  if [ "$ctx_before" = "$ctx_after" ]; then ok "docker context unchanged ($ctx_after)"; else bad "docker context moved" "$ctx_before -> $ctx_after"; fi

  rm -rf "$WORK"
  echo
  echo "fc-anywhere-e2e: $pass passed, $fail failed"
  [ "$fail" -eq 0 ]
}
trap 'cleanup; exit $?' EXIT

echo "== helper VM"
t0=$(date +%s)
if "$SBX" fc vm start; then ok "created and started in $(( $(date +%s) - t0 ))s"; else bad "sbx fc vm start failed"; exit 1; fi

if "$SBX" fc vm status | grep -q 'inside: active'; then ok "sbx serve --provider firecracker is active inside"; else bad "the in-VM daemon is not active" "$("$SBX" fc vm status)"; fi

echo "== host side"
osb_port=$(( 20000 + RANDOM % 10000 ))
"$SBX" serve --provider firecracker --osb-addr "127.0.0.1:$osb_port" >"$WORK/front.log" 2>&1 &
front_pid=$!

for _ in $(seq 1 120); do
  grep -q 'is serving' "$WORK/front.log" && break
  kill -0 "$front_pid" 2>/dev/null || break
  sleep 0.5
done

if grep -q 'is serving' "$WORK/front.log"; then ok "host front up"; else bad "host front never came up" "$(tail -5 "$WORK/front.log")"; exit 1; fi

if curl -fsS -o /dev/null "http://127.0.0.1:$osb_port/v1/sandboxes"; then
  ok "OpenSandbox API answers on the Mac"
else
  bad "OpenSandbox API did not answer on 127.0.0.1:$osb_port"
fi

echo "== a microVM sandbox, from the Mac"
if "$SBX" create "$NAME" --template nginx >"$WORK/create.log" 2>&1; then ok "sbx create --provider firecracker"; else bad "create failed" "$(tail -5 "$WORK/create.log")"; exit 1; fi

if "$SBX" list | grep -q "$NAME"; then ok "sbx list shows it"; else bad "sbx list does not show $NAME"; fi

eval "$("$SBX" env "$NAME" --shell posix)"
[ -n "${WEB_PORT:-}" ] && ok "sbx env: WEB_PORT=$WEB_PORT" || { bad "sbx env exported no WEB_PORT"; exit 1; }

# The mirror binds on its refresh tick; give it one.
for _ in $(seq 1 20); do nc -z 127.0.0.1 "$WEB_PORT" 2>/dev/null && break; sleep 0.5; done

# The wake: a plain TCP connect from the Mac, nothing sbx-specific on this side.
t0=$(perl -MTime::HiRes=time -e 'printf "%.3f", time')
status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 60 "http://127.0.0.1:$WEB_PORT/")
t1=$(perl -MTime::HiRes=time -e 'printf "%.3f", time')

if [ "$status" = 200 ]; then
  ok "TCP connect from the Mac woke it: HTTP 200 in $(perl -e "printf '%.0f', ($t1-$t0)*1000") ms"
else
  bad "wake over 127.0.0.1:$WEB_PORT answered $status"
fi

out=$("$SBX" exec "$NAME" nginx nginx -v 2>&1)
if printf '%s' "$out" | grep -q 'nginx version'; then ok "sbx exec: $out"; else bad "sbx exec" "$out"; fi

if "$SBX" logs "$NAME" nginx --tail 5 >/dev/null 2>&1; then ok "sbx logs"; else bad "sbx logs failed"; fi

out=$("$SBX" exec "$NAME" nginx cat /etc/hostname 2>&1)
if [ "$out" = nginx ]; then ok "sbx exec over vsock sees the guest's hostname"; else bad "sbx exec cat /etc/hostname" "$out"; fi

echo "== sleep (Diff snapshot) and wake (restore + re-key), $ROUNDS rounds"
# Each round sleeps the sandbox, wakes it with a TCP connect from the Mac and times the first
# byte, then times an awake request and an exec round trip - those two in alternating order
# (CONTRIBUTING: interleave and alternate). A create of a second sandbox (and its rm) is
# interleaved into every round, before or after the wake, alternating.
for k in create sleep wake warm exec; do : >"$WORK/$k.ms"; done

now_ms() { perl -MTime::HiRes=time -e 'printf "%.1f", time*1000'; }
sub_ms() { perl -e "printf '%.1f', $1 - $2"; }
s_to_ms() { perl -e "printf '%.1f', $1 * 1000"; }

create_round() {
  local n="$NAME-c$1" t0 t1
  t0=$(now_ms)
  if "$SBX" create "$n" --template nginx >"$WORK/create-$1.log" 2>&1; then
    t1=$(now_ms); sub_ms "$t1" "$t0" >>"$WORK/create.ms"; echo >>"$WORK/create.ms"
  else
    bad "round $1: create $n" "$(tail -3 "$WORK/create-$1.log")"
  fi
  "$SBX" rm "$n" >/dev/null 2>&1 || bad "round $1: rm $n"
}

warm() {
  local w; w=$(curl -s -o /dev/null -w '%{time_starttransfer}' --max-time 10 "http://127.0.0.1:$WEB_PORT/")
  s_to_ms "$w" >>"$WORK/warm.ms"; echo >>"$WORK/warm.ms"
}

execrt() {
  local a b o
  a=$(now_ms); o=$("$SBX" exec "$NAME" nginx true 2>&1); b=$(now_ms)
  if [ -z "$o" ]; then sub_ms "$b" "$a" >>"$WORK/exec.ms"; echo >>"$WORK/exec.ms"; else bad "round $1: exec" "$o"; fi
}

wake_round() {
  local t0 t1 fb
  t0=$(now_ms)
  "$SBX" sleep "$NAME" >"$WORK/sleep-$1.log" 2>&1 || { bad "round $1: sbx sleep" "$(tail -3 "$WORK/sleep-$1.log")"; return; }
  t1=$(now_ms); sub_ms "$t1" "$t0" >>"$WORK/sleep.ms"; echo >>"$WORK/sleep.ms"

  # `sbx sleep` stops the VM from outside the daemon, which notices on its next refresh (15s
  # by default). A connect before that is spliced to a guest address with nothing behind it and waits
  # out an ARP timeout before the daemon wakes it - a cost of this test's shortcut, not of a wake.
  sleep 16

  fb=$(curl -s -o /dev/null -w '%{http_code} %{time_starttransfer}' --max-time 60 "http://127.0.0.1:$WEB_PORT/")
  if [ "${fb%% *}" = 200 ]; then
    s_to_ms "${fb#* }" >>"$WORK/wake.ms"; echo >>"$WORK/wake.ms"
  else
    bad "round $1: wake answered ${fb%% *}"
  fi

  if [ $(( $1 % 2 )) -eq 1 ]; then warm; execrt "$1"; else execrt "$1"; warm; fi
}

for i in $(seq 1 "$ROUNDS"); do
  if [ $(( i % 2 )) -eq 1 ]; then wake_round "$i"; create_round "$i"; else create_round "$i"; wake_round "$i"; fi
done

# Median and p95 (nearest rank), in ms.
stats() {
  sort -n "$1" | awk 'NF {v[++n]=$1} END {
    if (n == 0) { print "n=0"; exit }
    m = (n % 2) ? v[(n+1)/2] : (v[n/2] + v[n/2+1]) / 2
    r = int(0.95 * n + 0.999999); if (r < 1) r = 1
    printf "n=%d median=%.1f p95=%.1f ms\n", n, m, v[r] }'
}

for k in create sleep wake warm exec; do
  line="$k: $(stats "$WORK/$k.ms")"
  echo "  $line"
  if [ -n "${FC_E2E_RESULTS:-}" ]; then echo "$line" >>"$FC_E2E_RESULTS"; fi
done

n_wake=$(grep -c . "$WORK/wake.ms")
if [ "$n_wake" -eq "$ROUNDS" ]; then ok "$ROUNDS/$ROUNDS wakes from a snapshot answered HTTP 200"; else bad "$n_wake/$ROUNDS wakes answered"; fi

if "$SBX" rm "$NAME" >/dev/null 2>&1; then ok "sbx rm"; else bad "sbx rm failed"; fi
