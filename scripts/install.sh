#!/usr/bin/env sh
# Install sbx.
#
#   curl -fsSL https://raw.githubusercontent.com/aryanmehrotra/sbx/main/scripts/install.sh | sh
#   VERSION=v0.1.0 DIR=~/bin sh install.sh
#
# POSIX sh, not bash: this is the one file that runs before anything is installed, on
# whatever the machine happens to have.
set -eu

REPO="${REPO:-aryanmehrotra/sbx}"
DIR="${DIR:-/usr/local/bin}"
VERSION="${VERSION:-latest}"

fail() { echo "install: $*" >&2; exit 1; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)

case "$arch" in
  x86_64|amd64)   arch=amd64 ;;
  arm64|aarch64)  arch=arm64 ;;
  *) fail "unsupported architecture: $arch" ;;
esac

case "$os" in
  darwin|linux|freebsd) ;;
  msys*|mingw*|cygwin*)
    # Named pipes need a dialer outside Go's standard library, so a native Windows build
    # cannot reach Docker Desktop. Saying so here is kinder than installing something that
    # runs and then cannot find a daemon.
    fail "on Windows, install inside WSL2 — sbx cannot dial a Windows named pipe" ;;
  *) fail "unsupported OS: $os" ;;
esac

if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
    sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$VERSION" ] || fail "could not determine the latest release; set VERSION explicitly"
fi

asset="sbx_${VERSION}_${os}_${arch}"
url="https://github.com/${REPO}/releases/download/${VERSION}/${asset}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "install: fetching ${asset}"
curl -fsSL "$url" -o "$tmp/sbx" || fail "no build published for ${os}/${arch} at ${VERSION}"

# Verified, not assumed. A binary fetched over the network and put on PATH is exactly the
# thing worth checking, and the checksums are published beside it.
if curl -fsSL "https://github.com/${REPO}/releases/download/${VERSION}/SHA256SUMS" -o "$tmp/SHA256SUMS" 2>/dev/null; then
  want=$(grep " $asset\$" "$tmp/SHA256SUMS" | awk '{print $1}')

  if [ -n "$want" ]; then
    if command -v shasum >/dev/null 2>&1; then got=$(shasum -a 256 "$tmp/sbx" | awk '{print $1}')
    else got=$(sha256sum "$tmp/sbx" | awk '{print $1}'); fi

    [ "$want" = "$got" ] || fail "checksum mismatch for ${asset}: expected $want, got $got"
    echo "install: checksum ok"
  else
    echo "install: WARNING — ${asset} is not listed in SHA256SUMS, so it was not verified" >&2
  fi
else
  # Said out loud. Skipping verification silently means an outage — or anything that can
  # block one small file while letting the binary through — defeats the check invisibly, and
  # a verified install then looks identical to an unverified one.
  echo "install: WARNING — could not fetch SHA256SUMS, so the binary was not verified" >&2
fi

chmod +x "$tmp/sbx"

# The destination may not exist yet — `DIR=~/bin`, which this script's own header suggests,
# is a directory plenty of machines do not have. Without this the move fails with a message
# about the file rather than about the directory.
if [ -d "$DIR" ] && [ -w "$DIR" ]; then
  mv "$tmp/sbx" "$DIR/sbx"
elif [ ! -e "$DIR" ] && mkdir -p "$DIR" 2>/dev/null; then
  mv "$tmp/sbx" "$DIR/sbx"
else
  echo "install: $DIR is not writable, using sudo"
  sudo mkdir -p "$DIR"
  sudo mv "$tmp/sbx" "$DIR/sbx"
fi

echo "install: sbx ${VERSION} -> ${DIR}/sbx"
"$DIR/sbx" --help 2>&1 | head -1
