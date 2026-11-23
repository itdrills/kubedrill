#!/usr/bin/env bash
# Renders packaging/homebrew/kubedrill.rb.tmpl for the local darwin tarball in dist/.
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${VERSION:-0.1.0}"
arch="$(uname -m)"; [ "$arch" = "x86_64" ] && arch=amd64
tarball="$PWD/dist/kubedrill_${VERSION}_darwin_${arch}.tar.gz"
[ -f "$tarball" ] || { echo "missing $tarball – run: make package" >&2; exit 1; }
sha="$(shasum -a 256 "$tarball" | awk '{print $1}')"
sed -e "s|@VERSION@|${VERSION}|g" -e "s|@URL@|file://${tarball}|g" -e "s|@SHA256@|${sha}|g" \
  packaging/homebrew/kubedrill.rb.tmpl > dist/kubedrill.rb
echo "wrote dist/kubedrill.rb (url file://${tarball}, sha256 ${sha})"
