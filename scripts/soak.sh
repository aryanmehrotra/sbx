#!/usr/bin/env bash
# Endurance: does the daemon leak under sustained connection churn?
#
#   scripts/soak.sh [seconds] [sandboxes]      # default 120s, 3 sandboxes
#   scripts/soak.sh 3600 5                      # a real overnight soak
#
# The unit tests prove no *races*; -race says nothing about a leak that only shows over time.
# The daemon is on the hot path - every connection to every sandbox is spliced through it - and
# connect.go says the risk out loud: a mishandled tunnel "leaks a goroutine and a file
# descriptor per dropped connection". So the signal is file descriptors: a leaked pipe goroutine
# holds a connection, which is an fd, so fd count is a faithful proxy for the goroutine leak, and
# both are visible from outside without linking anything into a daemon that has no dependencies.
# RSS catches a buffer or map that grows and never shrinks.
#
# It drives thousands of open/serve/close cycles (where the pipe goroutines and pool buffers are
# born and must be reaped), mixes in forced sleep/wake to churn the state machine, and samples fd
# and RSS *while quiescent* - after draining in-flight connections, so the number is the leak, not
# the work in progress. A leak is per-connection, so the count of connections driven is the
# denominator that makes one visible: flat fd over 5000 connections is the pass.
#
# One caveat it prints: `sbx serve` is one-per-machine, so it measures whatever daemon is running.
# On a machine also fronting other sandboxes, their activity is slow drift on top; a real
# per-connection leak dwarfs it. For a clean number, run this where the soak's daemon is the only
# one - a fresh VM, or a machine with nothing else pointed at it.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
DURATION="${1:-120}"
N="${2:-3}"
PREFIX="soak-$$"
BURST=20            # connections per sandbox per round
SAMPLE_EVERY=10     # seconds between quiescent samples
FD_TOL=8            # fds the quiescent count may grow by and still pass (pooling, not a leak)
RSS_TOL_KB=51200    # 50 MiB: buffer-pool warmup is a few MiB; a per-connection leak is not

[ -x "$SBX" ] || { echo "soak: build first: go build -o sbx ." >&2; exit 1; }

PID="$(pgrep -f 'sbx serve' | head -1)"
[ -n "$PID" ] || { echo "soak: no 'sbx serve' is running - start one, it is what this measures" >&2; exit 1; }

case "$(uname -s)" in
  Darwin) fds() { lsof -p "$PID" 2>/dev/null | wc -l | tr -d ' '; } ;;
  *)      fds() { find "/proc/$PID/fd" -mindepth 1 2>/dev/null | wc -l | tr -d ' '; } ;;
esac
rss() { ps -o rss= -p "$PID" 2>/dev/null | tr -d ' '; }   # KiB

pass=0; fail=0
ok()  { pass=$((pass + 1)); echo "  ✓ $1"; }
bad() { fail=$((fail + 1)); echo "  ✗ $1"; [ -n "${2:-}" ] && echo "      $2"; }

names=(); ports=()
cleanup() { for nm in "${names[@]}"; do "$SBX" rm "$nm" >/dev/null 2>&1; done; }
trap cleanup EXIT

echo "── setup: $N nginx sandboxes, daemon pid $PID ─────────────"
for i in $(seq 1 "$N"); do
  nm="$PREFIX-$i"
  "$SBX" create "$nm" --template nginx >/dev/null 2>&1
  p="$("$SBX" env "$nm" --shell json 2>/dev/null | grep -oE '"WEB_PORT":[^,}]*' | grep -oE '[0-9]+' | head -1)"
  [ -n "$p" ] || { bad "no WEB_PORT for $nm"; echo; echo "  $pass passed, $fail failed"; exit 1; }
  names+=("$nm"); ports+=("$p")
done

hit() { curl -s -o /dev/null --max-time 5 "http://127.0.0.1:$1/" 2>/dev/null; }

# Warmup: wake each once so the baseline includes every listener and the first pool buffers.
for p in "${ports[@]}"; do hit "$p"; done
sleep 2
base_fd="$(fds)"; base_rss="$(rss)"
ok "baseline captured: $base_fd fds, $((base_rss / 1024)) MiB RSS"

echo
echo "── churn for ${DURATION}s ──────────────────────────────────"
conns=0; peak_fd="$base_fd"; peak_rss="$base_rss"
start="$(date +%s)"; deadline=$((start + DURATION)); next=$((start + SAMPLE_EVERY))
printf "   %8s %8s %10s %12s\n" "t(s)" "conns" "fds" "RSS(MiB)"

while [ "$(date +%s)" -lt "$deadline" ]; do
  for idx in "${!ports[@]}"; do
    for _ in $(seq 1 "$BURST"); do hit "${ports[$idx]}"; conns=$((conns + 1)); done
  done
  # force a sleep on a random sandbox; the next round's hit re-wakes it - state-machine churn
  "$SBX" sleep "${names[$((RANDOM % N))]}" >/dev/null 2>&1

  now="$(date +%s)"
  if [ "$now" -ge "$next" ]; then
    sleep 1   # drain in-flight before sampling: the quiescent count is the leak signal
    f="$(fds)"; r="$(rss)"
    [ "$f" -gt "$peak_fd" ] && peak_fd="$f"
    [ "$r" -gt "$peak_rss" ] && peak_rss="$r"
    printf "   %8s %8s %10s %12s\n" "$((now - start))" "$conns" "$f" "$((r / 1024))"
    next=$((now + SAMPLE_EVERY))
  fi
done

# Final quiescent reading, well after the last connection.
sleep 3
fin_fd="$(fds)"; fin_rss="$(rss)"

echo
echo "── verdict over $conns connections ─────────────────────────"
echo "   fds:  baseline $base_fd  peak $peak_fd  final $fin_fd   (tolerance +$FD_TOL)"
echo "   RSS:  baseline $((base_rss/1024))  peak $((peak_rss/1024))  final $((fin_rss/1024)) MiB   (tolerance +$((RSS_TOL_KB/1024)))"

[ "$fin_fd" -le "$((base_fd + FD_TOL))" ] \
  && ok "fd count returned to baseline - no descriptor (goroutine) leak per connection" \
  || bad "fd count grew by $((fin_fd - base_fd)) over $conns connections - a per-connection leak" \
         "a leaked pipe goroutine holds a connection; see connect.go's tunnel teardown"

[ "$((fin_rss - base_rss))" -le "$RSS_TOL_KB" ] \
  && ok "RSS growth bounded - no unbounded buffer or map" \
  || bad "RSS grew $((( fin_rss - base_rss )/1024)) MiB over $conns connections" \
         "buffer-pool warmup is a few MiB; this is more"

echo
echo "  $pass passed, $fail failed  ($conns connections in ${DURATION}s)"
[ "$fail" -eq 0 ] || exit 1
