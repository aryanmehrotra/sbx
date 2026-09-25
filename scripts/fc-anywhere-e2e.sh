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
  "$SBX" rm "$NAME" >/dev/null 2>&1 || true
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

if "$SBX" rm "$NAME" >/dev/null 2>&1; then ok "sbx rm"; else bad "sbx rm failed"; fi
