#!/usr/bin/env bash
# Builds Homeport for Linux (amd64 and arm64) into dist/, with the installer and checksums, ready for a GitHub
# release or to copy to a server.
set -euo pipefail
cd "$(dirname "$0")"
version="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
rm -rf dist && mkdir -p dist
for os in linux darwin; do
  for arch in amd64 arm64; do
    CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$version" -o "dist/homeport-$os-$arch" ./cmd/homeport
  done
done
cp install.sh install-mac.sh dist/
(cd dist && shasum -a 256 homeport-linux-* > SHA256SUMS)
echo "Built Homeport $version in dist/"
ls -la dist
