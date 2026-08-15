#!/usr/bin/env bash
# Build every published binary, reproducibly, with nothing installed but Go.
#
#   scripts/release.sh v0.1.0
#
# There is no goreleaser here on purpose. This tool's claim is that it has no dependencies,
# and a release process that needs a release tool to be installed first is a worse version of
# the same problem it exists to avoid. Fifteen lines of `go build` does the job.
#
# Output: dist/sbx_<version>_<os>_<arch>[.exe] plus SHA256SUMS, which is what install.sh
# reads and what a package manager would verify.
set -euo pipefail

VERSION="${1:?usage: release.sh vX.Y.Z}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"

# windows/* is built and published, but it is only useful under WSL2: dialling a named pipe
# needs a dialer outside the standard library. Shipping it anyway means `go install` and any
# future TCP-endpoint use keep working there.
TARGETS="
darwin/arm64
darwin/amd64
linux/amd64
linux/arm64
windows/amd64
windows/arm64
freebsd/amd64
freebsd/arm64
"

rm -rf "$DIST"
mkdir -p "$DIST"

cd "$ROOT"

for t in $TARGETS; do
  os="${t%/*}"; arch="${t#*/}"
  out="$DIST/sbx_${VERSION}_${os}_${arch}"
  [ "$os" = "windows" ] && out="${out}.exe"

  # CGO off is what makes these static and portable across libc versions; trimpath and the
  # zeroed build id are what make two builds of the same commit identical.
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION} -buildid=" -o "$out" .

  printf '  %-28s %s\n' "$(basename "$out")" "$(wc -c < "$out" | tr -d ' ') bytes"
done

cd "$DIST"

if command -v shasum >/dev/null 2>&1; then
  shasum -a 256 sbx_* > SHA256SUMS
else
  sha256sum sbx_* > SHA256SUMS
fi

echo
echo "dist/ ready for ${VERSION}:"
cat SHA256SUMS
