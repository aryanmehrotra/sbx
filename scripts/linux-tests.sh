#!/usr/bin/env bash
# Run sbx's own test suite on Linux, in an sbx sandbox.
#
# sbx is developed on a Mac and shipped for Linux, and the two differ where it matters most:
# the daemon binds a bridge gateway that only exists natively on Linux, CRIU is Linux-only, and
# the container runtime is a VM here and the kernel there. A suite that has only ever run on
# darwin is a suite that has never seen the platform most of its users are on.
#
# It is also the shortest honest demo of the tool: a sandbox that holds a toolchain, sleeps to
# 0 B when nobody is compiling, and wakes on the next `sbx exec`.
#
#   scripts/linux-tests.sh                # vet, then the suite with -race
#   scripts/linux-tests.sh -- -run TestX  # pass anything else through to go test
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SBX="${SBX_BIN:-$ROOT/sbx}"
SPEC="$ROOT/.sbx-linux-tests.json"
IMAGE="${SBX_LINUX_IMAGE:-golang:1.26-trixie}"

cd "$ROOT"

[ -x "$SBX" ] || { echo "build first: go build -o sbx ." >&2; exit 1; }

# The worktree's own sandbox, whatever it is called here. Never a name of our own choosing:
# a sandbox named off-convention is one nothing collects.
NAME="${SBX_SANDBOX:-$(dw --sbx 2>/dev/null || true)}"
[ -n "$NAME" ] || { echo "no sandbox for this worktree; set SBX_SANDBOX=<name>" >&2; exit 1; }

# A long-lived listener so the service has a port to be woken on, and nothing else. The build
# and module caches are a volume, so a cold second run is not a second download.
cat > "$SPEC" <<JSON
{
  "version": 1,
  "services": {
    "go": {
      "image": "$IMAGE",
      "ports": [7777],
      "args": ["sh", "-c", "while true; do nc -l -p 7777 >/dev/null 2>&1 || sleep 1; done"],
      "health": "true",
      "mounts": { "$ROOT": "/src" },
      "env": { "GOCACHE": "/gocache", "GOMODCACHE": "/gocache/mod" },
      "volume": "/gocache",
      "memory": "4g"
    }
  }
}
JSON
trap 'rm -f "$SPEC"' EXIT

"$SBX" create "$NAME" --spec "$SPEC" >/dev/null 2>&1 || true

echo "==> $(  "$SBX" exec "$NAME" go go version 2>&1 | tail -1 )"

echo "==> go vet"
"$SBX" exec "$NAME" go sh -c 'cd /src && go vet ./...'

echo "==> go test -race ${*:-}"
"$SBX" exec "$NAME" go sh -c "cd /src && go test ./... -race -count=1 ${*:-} 2>&1 | grep -v 'no test files'"

echo
echo "the sandbox stays. It sleeps to 0 B on the idle timer and wakes on the next exec."
