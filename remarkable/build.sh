#!/bin/sh
# Cross-compile newt for both reMarkable tablets into remarkable/out.
#
#   remarkable/build.sh            # arm64 (Paper Pro) and arm32 (reMarkable 1 and 2)
#   VERSION=1.2.3 remarkable/build.sh
set -eu
cd "$(dirname "$0")/.."
VERSION=${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)-remarkable}
OUT=remarkable/out
mkdir -p "$OUT"
ld="-s -w -X main.newtVersion=$VERSION"
# Paper Pro: aarch64, static Go binary, no cgo
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$ld -X main.newtPlatform=linux_arm64" -o "$OUT/newt_linux_arm64"
# reMarkable 1 and 2: armv7 hard-float
CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=7 go build -ldflags "$ld -X main.newtPlatform=linux_arm32" -o "$OUT/newt_linux_arm32"
ls -la "$OUT"
