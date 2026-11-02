#!/usr/bin/env bash
# Build release archives for scripts/release.sh: one tar.gz per platform plus checksums.txt.
# Each archive holds BINARY_VERSION_OS_ARCH/{BINARY,LICENSE,README.md}, the layout the
# Homebrew formula and scripts/install.sh expect.
set -euo pipefail
version="${1:?usage: build-assets.sh vX.Y.Z}"
binary="vectordb"
ldflags="-s -w -buildid= -X github.com/adeoluo/vectordb-go/internal/cli.Version=$version"
out=dist/release
rm -rf "$out" && mkdir -p "$out"
for platform in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  os="${platform%/*}"; arch="${platform#*/}"
  name="${binary}_${version}_${os}_${arch}"
  mkdir -p "$out/$name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$out/$name/$binary" "./cmd/$binary"
  cp LICENSE README.md "$out/$name/"
  COPYFILE_DISABLE=1 tar -C "$out" -czf "$out/$name.tar.gz" "$name"
  rm -rf "${out:?}/$name"
done
(cd "$out" && shasum -a 256 ./*.tar.gz | sed 's# \./# #' > checksums.txt)
echo "assets in $out"
