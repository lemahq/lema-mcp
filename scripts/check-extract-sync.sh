#!/usr/bin/env bash
#
# Guard against publishing stale bits: this dist's extracted Go source MUST match
# a fresh extraction from the monorepo — the single source of truth (ADR-0033 §3).
# Run by scripts/publish.sh before it builds/publishes. If it reports drift, the
# fix is to re-run the monorepo's scripts/extract-lema-mcp.sh, then publish again.
#
#   Bypass (intentional, rare):       LEMA_SKIP_EXTRACT_CHECK=1
#   Point at a non-default monorepo:  LEMA_MONOREPO=/path/to/lema
set -euo pipefail

DIST="$(cd "$(dirname "$0")/.." && pwd)"
MONOREPO="${LEMA_MONOREPO:-$DIST/../lema}"

if [ "${LEMA_SKIP_EXTRACT_CHECK:-0}" = "1" ]; then
	echo "drift check: SKIPPED (LEMA_SKIP_EXTRACT_CHECK=1)"
	exit 0
fi
if [ ! -d "$MONOREPO/apps/api" ]; then
	echo "drift check: SKIPPED (monorepo not found at $MONOREPO; set LEMA_MONOREPO to enable)" >&2
	exit 0
fi

SRC="$MONOREPO/apps/api"
OLD="github.com/lemahq/lema/apps/api"
NEW="github.com/lemahq/lema-mcp"
# The exact set the monorepo extracts (kept in lockstep with extract-lema-mcp.sh).
DIRS="cmd/lema-mcp internal/adr internal/source internal/openspec"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Build a normalized image of what a fresh extraction would produce (the same
# module-path rewrite extract-lema-mcp.sh applies), then diff it against dist.
drift=0
for d in $DIRS; do
	mkdir -p "$TMP/$d"
	for f in "$SRC/$d"/*.go; do
		sed "s#$OLD#$NEW#g" "$f" >"$TMP/$d/$(basename "$f")"
	done
	if ! diff -rq "$TMP/$d" "$DIST/$d" >/dev/null 2>&1; then
		echo "  ✗ drift in $d:"
		diff -rq "$TMP/$d" "$DIST/$d" || true
		drift=1
	fi
done

if [ "$drift" = 1 ]; then
	cat >&2 <<EOF

✗ ABORT: this dist's Go source has drifted from the monorepo.
  The monorepo is the source of truth (ADR-0033 §3) — do not hand-edit dist Go.
  Fix:    bash $MONOREPO/scripts/extract-lema-mcp.sh
  Bypass: LEMA_SKIP_EXTRACT_CHECK=1 bash scripts/publish.sh
EOF
	exit 1
fi

echo "drift check: OK — extracted Go source matches the monorepo"
