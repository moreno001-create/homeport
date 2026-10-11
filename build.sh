#!/usr/bin/env bash
# Builds Homeport for Linux (amd64 and arm64) into dist/, with the installer, ready to copy to a server.
set -euo pipefail
cd "$(dirname "$0")"
version="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
rm -rf dist && mkdir -p dist
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=$version" -o "dist/homeport-linux-$arch" ./cmd/homeport
done
cp packaging/install.sh packaging/homeport.service dist/
echo "Built Homeport $version in dist/"
ls -la dist
