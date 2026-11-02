#!/bin/sh
# Install the latest vectordb release binary.
#   curl -fsSL https://raw.githubusercontent.com/adeoluo/vectordb-go/main/scripts/install.sh | sh
# Environment: VERSION (default: latest release), INSTALL_DIR (default: $HOME/.local/bin).
set -eu
repo="adeoluo/vectordb-go"
binary="vectordb"
install_dir="${INSTALL_DIR:-$HOME/.local/bin}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
case "$os" in darwin|linux) ;; *) echo "unsupported OS: $os" >&2; exit 1 ;; esac

version="${VERSION:-$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p')}"
[ -n "$version" ] || { echo "could not resolve the latest release" >&2; exit 1; }
name="${binary}_${version}_${os}_${arch}"
base="https://github.com/$repo/releases/download/$version"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
if ! curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz"; then
  echo "no prebuilt $binary for $os/$arch in $version" >&2; exit 1
fi
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
expected="$(grep " $name.tar.gz\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
if command -v sha256sum >/dev/null; then actual="$(sha256sum "$tmp/$name.tar.gz" | cut -d' ' -f1)"
else actual="$(shasum -a 256 "$tmp/$name.tar.gz" | cut -d' ' -f1)"; fi
[ -n "$expected" ] && [ "$expected" = "$actual" ] || { echo "checksum mismatch for $name.tar.gz" >&2; exit 1; }

tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
mkdir -p "$install_dir"
install -m 0755 "$tmp/$name/$binary" "$install_dir/$binary"
echo "installed $binary $version to $install_dir/$binary"
case ":$PATH:" in *":$install_dir:"*) ;; *) echo "add $install_dir to PATH to use it" ;; esac
