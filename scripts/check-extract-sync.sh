#!/usr/bin/env bash
#
# Guard against publishing stale bits: this dist's extracted Go source MUST match
# a fresh extraction from the monorepo — the single source of truth (ADR-0033 §3).
# Run by scripts/publish.sh before it builds/publishes. On drift the remedy depends
# on DIRECTION: re-extract if the monorepo is ahead, but PORT BACK first if dist has
# Go the monorepo lacks (e.g. a fix merged here via PR) — re-extract OVERWRITES dist Go.
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
DIRS="cmd/lema-mcp internal/adr internal/source internal/openspec internal/docs internal/verdict"

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
		# Relabel the temp/dist paths so the direction of each drift is legible:
		# "Only in (dist)/…" is the dangerous case a re-extract would delete.
		diff -rq "$TMP/$d" "$DIST/$d" 2>&1 | sed "s#$TMP#(monorepo)#g; s#$DIST#(dist)#g" || true
		drift=1
	fi
done

if [ "$drift" = 1 ]; then
	cat >&2 <<EOF

✗ ABORT: this dist's Go source has drifted from the monorepo (source of truth, ADR-0033 §3).
  The remedy depends on WHICH WAY it drifted — read the lines above:

  • "Files … differ" or "Only in (monorepo)/…"  →  the monorepo is ahead.
      Fix by re-extracting:  bash $MONOREPO/scripts/extract-lema-mcp.sh

  • "Only in (dist)/…"  →  that file exists ONLY in dist (e.g. a fix merged here via a
    GitHub PR that never made it back to the monorepo). Re-extract OVERWRITES dist's Go
    and would DELETE it. Port it INTO the monorepo FIRST, then re-extract — do NOT
    blindly re-extract.

  Bypass (only if you are certain dist is in sync): LEMA_SKIP_EXTRACT_CHECK=1 bash scripts/publish.sh
EOF
	exit 1
fi

echo "drift check: OK — extracted Go source matches the monorepo"
