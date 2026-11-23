#!/usr/bin/env bash
# Cross-compiles release tarballs into ./dist and writes checksums.txt.
# Usage: scripts/package.sh <version> "<os/arch> <os/arch> ..."
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${1:?version required}"
PLATFORMS="${2:-darwin/arm64 darwin/amd64 linux/amd64 linux/arm64}"
mkdir -p dist
for p in $PLATFORMS; do
  os="${p%/*}"; arch="${p#*/}"
  name="kubedrill_${VERSION}_${os}_${arch}"
  stage="dist/${name}"
  echo "building ${name}"
  mkdir -p "$stage"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" -o "${stage}/kubedrill" ./cmd/kubedrill
  cp LICENSE README.md "$stage/"
  tar -C "$stage" -czf "dist/${name}.tar.gz" kubedrill LICENSE README.md
  rm -r -- "$stage"
done
(cd dist && shasum -a 256 ./*.tar.gz | sed 's| \./| |' > checksums.txt && cat checksums.txt)
