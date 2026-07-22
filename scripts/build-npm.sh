#!/usr/bin/env bash
#
# Cross-compile the lema-mcp Go binary into each per-platform npm package.
# lema-mcp is pure Go (CGO-free), so cross-compilation is just GOOS/GOARCH — no
# platform-specific build machines. Run before `npm publish` (see PUBLISHING.md).
#
# The binaries are gitignored; this script (re)creates them at publish time so
# the repo stays small.
set -euo pipefail

DIST="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="$(node -p "require('$DIST/npm/lema-mcp/package.json').version")"
echo "building lema-mcp $VERSION binaries (pure Go, cross-compiled)"

build() {
	local dir="$1" goos="$2" goarch="$3" out
	out="$DIST/npm/$dir/bin/lema-mcp"
	[ "$goos" = "windows" ] && out="${out}.exe"
	mkdir -p "$(dirname "$out")"
	echo "  → $dir ($goos/$goarch)"
	( cd "$DIST" && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
		go build -trimpath -ldflags "-s -w -X main.Version=$VERSION" -o "$out" ./cmd/lema-mcp )
}

build darwin-arm64 darwin arm64
build darwin-x64   darwin amd64
build linux-x64    linux  amd64
build linux-arm64  linux  arm64

echo "done. binaries in npm/<platform>/bin/ — publish with scripts in PUBLISHING.md"
