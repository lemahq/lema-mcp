#!/usr/bin/env bash
#
# One-shot publish for lema-mcp. Builds the per-platform binaries, then publishes
# the platform packages FIRST (the launcher's optionalDependencies must already
# exist on the registry) and the `lema-mcp` launcher LAST. Public npm — free.
#
# Requires a logged-in npm account that owns `lema-mcp`:
#   npm login
#   bash scripts/publish.sh
set -euo pipefail

DIST="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="$(node -p "require('$DIST/npm/lema-mcp/package.json').version")"

echo "lema-mcp publish $VERSION"

echo "→ building binaries"
bash "$DIST/scripts/build-npm.sh"

echo "→ publishing platform packages"
for d in darwin-arm64 darwin-x64 linux-x64 linux-arm64; do
	echo "   lema-mcp-$d@$VERSION"
	( cd "$DIST/npm/$d" && npm publish --access public )
done

echo "→ publishing launcher (last)"
( cd "$DIST/npm/lema-mcp" && npm publish --access public )

echo ""
echo "✓ published lema-mcp@$VERSION. Verify on a clean machine:"
echo "    npx lema-mcp@latest demo      # the 30-second never-reopen walkthrough"
echo "    npx lema-mcp@latest init      # wire it into a repo"
