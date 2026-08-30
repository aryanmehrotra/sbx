#!/usr/bin/env bash
# Build and vet sbx for every platform it claims to support.
#
# sbx is written on a Mac and shipped for Linux, and its users are on both plus Windows. Almost
# nothing here is platform-specific - but "almost" is the problem: the terminal handling, the raw
# mode, the socket paths and the CRIU refusal all have per-OS files, and a build tag that stops
# matching produces a package that simply does not compile over there. Nothing in a local test run
# says so, because a local test run is one GOOS.
#
# Vet as well as build, and vet the TESTS too: `go vet` type-checks _test.go files, which `go
# build` does not. That is how the Windows gap was found - the production code compiled there
# perfectly well and the test suite did not, so nobody on Windows could have run it.
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

TARGETS=(
  linux/amd64 linux/arm64 linux/386
  darwin/amd64 darwin/arm64
  windows/amd64 windows/arm64
  freebsd/amd64
)

fail=0

for t in "${TARGETS[@]}"; do
  os=${t%/*}; arch=${t#*/}

  if out=$(GOOS=$os GOARCH=$arch go build -o /dev/null ./... 2>&1); then
    build=ok
  else
    build=FAIL; fail=1
    printf '  %-16s build FAILED\n%s\n' "$t" "$out"
  fi

  # Vet covers the tests, which build does not.
  if out=$(GOOS=$os GOARCH=$arch go vet ./... 2>&1); then
    vet=ok
  else
    vet=FAIL; fail=1
    printf '  %-16s vet FAILED\n%s\n' "$t" "$out"
  fi

  [ "$build $vet" = "ok ok" ] && printf '  %-16s build ok   vet ok\n' "$t"
done

echo
if [ "$fail" -eq 0 ]; then
  echo "  ${#TARGETS[@]} platforms build and vet clean"
else
  echo "  some platforms failed"
fi

exit "$fail"
