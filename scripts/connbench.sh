#!/usr/bin/env bash
# What a new connection costs once the sandbox is already awake.
#
#   scripts/connbench.sh [connections]     # default 20
#
# This is the third number, and for a while it was the missing one. Two were already
# measured: the wake (how long the first caller waits for a sleeping sandbox) and the
# steady-state proxy tax measured by the Go benchmark next door (~33 µs per round trip on an
# already-open connection).
#
# Neither covers the case that turns out to be the common one. A client that opens a
# connection per operation — psql, redis-cli, any CLI, any client without a pool — is not
# paying the wake, because the sandbox is awake, and is not paying only the round-trip tax
# either, because it opens a new connection each time. That path went unmeasured, and it was
# costing 68 ms a connection: the daemon re-ran the health command through `docker exec` on
# every accepted connection, having already been told the answer.
#
# Both sides of the same PING, interleaved, on the same awake container: through the daemon's
# public port, and straight to the port docker published. The difference is the daemon's
# per-connection cost and nothing else — same workload, same machine, same moment.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="$ROOT/sbx"
N="${1:-20}"
NAME="connbench$$"
WORK="$(mktemp -d)"
DAEMON=""

cleanup() {
  [ -n "$DAEMON" ] && kill "$DAEMON" 2>/dev/null
  "$SBX" rm "$NAME" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

[ -x "$SBX" ] || { echo "build first: go build -o sbx ." >&2; exit 1; }
docker info >/dev/null 2>&1 || { echo "needs a docker daemon" >&2; exit 1; }

# Redis for the same reason bench.sh uses it: the cheapest correct protocol reply available,
# so what is measured is the connection path rather than the workload's own parsing.
cat > "$WORK/spec.json" <<'JSON'
{ "version": 1, "services": { "redis": { "image": "redis:7-alpine", "ports": [6379],
  "health": "redis-cli ping" } }, "exports": { "REDIS_PORT": "redis:6379" } }
JSON

echo
echo "── conditions ──────────────────────────────────────────────"
printf '  %-14s %s\n' "host" "$(uname -sm)"
printf '  %-14s %s\n' "connections" "$N each side"

"$SBX" serve --idle 30m --refresh 3s >"$WORK/daemon.log" 2>&1 &
DAEMON=$!
sleep 3

"$SBX" create "$NAME" --spec "$WORK/spec.json" >/dev/null 2>&1 || {
  echo "could not create the sandbox" >&2
  exit 1
}

# shellcheck disable=SC1090
eval "$("$SBX" env "$NAME" --spec "$WORK/spec.json" 2>/dev/null)"

BACKING=$(docker port "sbx-$NAME-redis" 6379/tcp 2>/dev/null | head -1 | sed 's/.*://')
[ -n "${REDIS_PORT:-}" ] && [ -n "$BACKING" ] || { echo "could not resolve both ports" >&2; exit 1; }

# Awake, and settled. This measures the steady state, not a wake — a wake here would be the
# 191 ms number wearing this one's name.
redis-cli -p "$REDIS_PORT" ping >/dev/null 2>&1
sleep 2

echo
echo "── a new connection, awake sandbox ─────────────────────────"

python3 - "$REDIS_PORT" "$BACKING" "$N" <<'PY'
import socket, statistics, sys

def one(port):
    import time
    t0 = time.perf_counter()
    s = socket.create_connection(("127.0.0.1", port), timeout=30)
    s.sendall(b"PING\r\n")
    reply = s.recv(64)
    s.close()
    # A sample counts only on a correct protocol reply — the same rule the rest of the
    # harness uses. A refused or empty read is not a fast connection.
    if not reply.startswith(b"+PONG"):
        raise SystemExit(f"not a redis reply: {reply!r}")
    return (time.perf_counter() - t0) * 1000

pub, back, n = int(sys.argv[1]), int(sys.argv[2]), int(sys.argv[3])

# Interleaved, so a machine that gets busy halfway through spoils both sides equally
# instead of handing the win to whichever ran first.
through, direct = [], []
for _ in range(n):
    through.append(one(pub))
    direct.append(one(back))

def show(label, xs):
    xs = sorted(xs)
    print(f"  {label:<22} median {statistics.median(xs):7.2f} ms   "
          f"min {xs[0]:6.2f}   max {xs[-1]:6.2f}")
    return statistics.median(xs)

t = show("through the daemon", through)
d = show("straight to docker", direct)

delta = t - d
jitter = max(max(through) - min(through), max(direct) - min(direct))

print()
print(f"  per-connection cost   {delta:+.2f} ms")

# The same refusal the rest of the harness makes: a delta smaller than the run-to-run
# spread is not a measurement, and reporting it as one is how a number becomes folklore.
if abs(delta) < jitter:
    print(f"  → below the {jitter:.2f} ms jitter on this machine: indistinguishable from zero.")
else:
    print(f"  → above the {jitter:.2f} ms jitter: a real cost, and it should not be here.")
PY
