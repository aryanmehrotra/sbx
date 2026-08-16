#!/usr/bin/env bash
# Build every published binary with nothing installed but Go, reproducibly from a clean
# checkout on a matching Go version - see the note on the build below for both qualifiers.
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
  # zeroed build id remove the two things that would otherwise differ between machines - the
  # build path and a random id.
  #
  # Two conditions have to hold for the bytes to match, and both are things Go stamps into the
  # binary rather than anything this script controls:
  #
  #   the same Go version      1.26.3 and 1.26.5 give different checksums for one commit.
  #                            release.yaml pins an exact patch so re-running a tag reproduces
  #   a clean checkout         a modified tree is stamped vcs.modified=true and the module
  #                            version becomes v0.1.0+dirty. Building from your working copy
  #                            will not match a release even when nothing you changed is
  #                            compiled in
  #
  # The host does not have to match: darwin/arm64 and linux/amd64 both produce the published
  # linux/amd64 binary bit for bit. Verified against v0.1.0 rather than assumed.
  #
  #   git clone --branch vX.Y.Z --depth 1 <repo> && cd <repo>
  #   go version -m ./sbx_vX.Y.Z_linux_amd64   # read the toolchain out of the release binary
  #   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 goX.Y.Z build -trimpath \
  #     -ldflags "-s -w -X main.version=vX.Y.Z -buildid=" -o sbx .
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
