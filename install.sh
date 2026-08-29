#!/bin/sh
# limitping installer — downloads and verifies the right prebuilt binary from
# the latest GitHub release. No Go required.
#
# Review this script locally before running it. Override the release source with
# LIMITPING_REPO=owner/repo when testing another trusted fork.
#
# Override the install directory with LIMITPING_INSTALL_DIR=/path sh install.sh
set -eu

REPO="${LIMITPING_REPO:-ashgreat/CCLimitPing}"
BIN="limitping"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64 | amd64) arch="amd64" ;;
  arm64 | aarch64) arch="arm64" ;;
  *) echo "limitping: unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  darwin | linux) ;;
  *) echo "limitping: unsupported OS: $os (build from source: go build ./cmd/limitping)" >&2; exit 1 ;;
esac

asset="${BIN}_${os}_${arch}.tar.gz"
url="https://github.com/${REPO}/releases/latest/download/${asset}"
checksum_url="https://github.com/${REPO}/releases/latest/download/checksums.txt"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

download() {
  source_url=$1
  destination=$2
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$source_url" -o "$destination"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$destination" "$source_url"
  else
    echo "limitping: need curl or wget" >&2; exit 1
  fi
}

echo "Downloading ${url}"
download "$url" "$tmp/$asset"
download "$checksum_url" "$tmp/checksums.txt"

expected=$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1; exit }' "$tmp/checksums.txt")
if [ -z "$expected" ]; then
  echo "limitping: ${asset} is missing from the published release checksums" >&2
  exit 1
fi
if command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
elif command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$asset" | awk '{print $1}')
else
  echo "limitping: need shasum or sha256sum to verify the download" >&2
  exit 1
fi
if [ "$actual" != "$expected" ]; then
  echo "limitping: checksum verification failed for ${asset}" >&2
  exit 1
fi
echo "Verified SHA-256 checksum"

tar -xzf "$tmp/$asset" -C "$tmp"

if [ -n "${LIMITPING_INSTALL_DIR:-}" ]; then
  dir="$LIMITPING_INSTALL_DIR"
elif [ -w /usr/local/bin ]; then
  dir="/usr/local/bin"
else
  dir="$HOME/.local/bin"
fi
mkdir -p "$dir"

if cp "$tmp/$BIN" "$dir/$BIN" 2>/dev/null; then
  chmod 0755 "$dir/$BIN"
else
  echo "limitping: cannot write to $dir; retrying with sudo"
  sudo cp "$tmp/$BIN" "$dir/$BIN"
  sudo chmod 0755 "$dir/$BIN"
fi

echo "Installed $BIN -> $dir/$BIN"
case ":$PATH:" in
  *":$dir:"*) ;;
  *)
    echo
    echo "NOTE: $dir is not on your PATH. Add it, e.g.:"
    echo "  export PATH=\"$dir:\$PATH\""
    ;;
esac

echo
echo "No Claude/Codex settings were modified. Active-session hooks are optional:"
echo "  limitping hooks install claude"
if [ "$os" = "darwin" ]; then
  echo "Persistent macOS service (Claude only by default):"
  echo "  limitping service install claude"
fi

"$dir/$BIN" version 2>/dev/null || true
