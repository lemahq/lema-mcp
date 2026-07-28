#!/usr/bin/env bash
#
# Guard against publishing stale bits: this dist's extracted Go source MUST match
# a fresh extraction from the monorepo — the source of truth for the SHARED
# INTERNAL PACKAGES (ADR-0033 §3). Run by scripts/publish.sh before it builds/
# publishes. On drift, "Files … differ" alone carries NO direction — it just means
# the bytes differ. Direction is resolved per file below from git commit dates on
# both sides, because re-extracting is destructive (rm -rf + cp, no merge step):
# re-extracting a dist-ahead file DELETES work that only exists here.
#
# cmd/lema-mcp is NOT checked: this repo OWNS it since the pivot B2 entry gate
# (D1 7978b83e) retired the monorepo copy — there is nothing to compare against.
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
# The exact set the monorepo extracts (kept in lockstep with extract-lema-mcp.sh's
# GO_DIRS — lemahq/lema#430: the two lists must stay identical, same order).
DIRS="internal/adr internal/source internal/openspec internal/docs internal/verdict internal/decisioncheck internal/httpx"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

MONOREPO_AHEAD=()
DIST_AHEAD=()
UNRESOLVED=()

# Resolve which side is ahead for one drifted file, using each side's last commit
# date for that path. Sets CLASS_LABEL (human-readable) and CLASS_BUCKET (one of
# MONOREPO_AHEAD / DIST_AHEAD / UNRESOLVED). Must run in the current shell (not a
# command substitution) so callers can still see side effects if they add any.
classify_file() {
	local relpath="$1"
	local mono_line dist_line mono_ts dist_ts mono_date dist_date
	mono_line="$(git -C "$MONOREPO" log -1 --format='%ct|%cI' -- "apps/api/$relpath" 2>/dev/null || true)"
	dist_line="$(cd "$DIST" && git log -1 --format='%ct|%cI' -- "$relpath" 2>/dev/null || true)"
	mono_ts="${mono_line%%|*}"
	mono_date="${mono_line#*|}"
	dist_ts="${dist_line%%|*}"
	dist_date="${dist_line#*|}"
	# Shallow history or a path that predates a rename can yield an empty date on
	# either side — never let an empty date sort as "oldest". Treat as unresolved.
	if [ -z "$mono_ts" ] || [ -z "$dist_ts" ]; then
		CLASS_LABEL="same-date — resolve by hand (no git history for this path on one side: monorepo='${mono_date}' dist='${dist_date}')"
		CLASS_BUCKET="UNRESOLVED"
	elif [ "$mono_ts" -gt "$dist_ts" ]; then
		CLASS_LABEL="monorepo ahead (monorepo $mono_date > dist $dist_date)"
		CLASS_BUCKET="MONOREPO_AHEAD"
	elif [ "$dist_ts" -gt "$mono_ts" ]; then
		CLASS_LABEL="dist ahead (dist $dist_date > monorepo $mono_date)"
		CLASS_BUCKET="DIST_AHEAD"
	else
		CLASS_LABEL="same-date — resolve by hand (identical commit timestamps)"
		CLASS_BUCKET="UNRESOLVED"
	fi
}

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
		while IFS= read -r line; do
			case "$line" in
			"Only in $TMP/$d:"*)
				# Exists only in the monorepo's fresh extraction image — not yet
				# copied into dist. Existence alone is unambiguous: monorepo ahead.
				fname="${line#"Only in $TMP/$d: "}"
				relpath="$d/$fname"
				echo "    (monorepo)/$relpath — only in monorepo → monorepo ahead"
				MONOREPO_AHEAD+=("$relpath")
				;;
			"Only in $DIST/$d:"*)
				# Exists only in dist (e.g. merged here via a PR that never made
				# it back to the monorepo). Existence alone is unambiguous: dist
				# ahead — a re-extract would delete it.
				fname="${line#"Only in $DIST/$d: "}"
				relpath="$d/$fname"
				echo "    (dist)/$relpath — only in dist → dist ahead"
				DIST_AHEAD+=("$relpath")
				;;
			"Files $TMP/$d/"*" differ")
				# Both sides have the file and the bytes differ. This carries NO
				# direction by itself — resolve it from commit dates.
				rest="${line#"Files $TMP/$d/"}"
				fname="${rest%% and *}"
				relpath="$d/$fname"
				classify_file "$relpath"
				echo "    (monorepo)/$relpath and (dist)/$relpath differ → $CLASS_LABEL"
				case "$CLASS_BUCKET" in
				MONOREPO_AHEAD) MONOREPO_AHEAD+=("$relpath") ;;
				DIST_AHEAD) DIST_AHEAD+=("$relpath") ;;
				UNRESOLVED) UNRESOLVED+=("$relpath") ;;
				esac
				;;
			*)
				echo "    $line"
				;;
			esac
		done < <(diff -rq "$TMP/$d" "$DIST/$d" 2>&1 || true)
		drift=1
	fi
done

if [ "$drift" = 1 ]; then
	{
		echo
		echo "✗ ABORT: this dist's Go source has drifted from the monorepo (source of truth, ADR-0033 §3)."
		echo "  Direction was resolved per file above from each side's last commit date. The remedy"
		echo "  depends on direction — re-extracting is destructive (rm -rf + cp, no merge step)."
		echo

		if [ "${#MONOREPO_AHEAD[@]}" -gt 0 ]; then
			echo "  MONOREPO AHEAD — safe to re-extract:"
			for f in "${MONOREPO_AHEAD[@]}"; do echo "    - $f"; done
			echo "    Fix:  bash $MONOREPO/scripts/extract-lema-mcp.sh"
			echo
		fi

		if [ "${#DIST_AHEAD[@]}" -gt 0 ]; then
			echo "  DIST AHEAD — do NOT re-extract these yet. Port them INTO the monorepo FIRST:"
			for f in "${DIST_AHEAD[@]}"; do echo "    - $f"; done
			echo "    Re-extracting now would silently DELETE this dist-only work."
			echo
		fi

		if [ "${#UNRESOLVED[@]}" -gt 0 ]; then
			echo "  UNRESOLVED — git history didn't give a clear direction; resolve by hand (read the diff):"
			for f in "${UNRESOLVED[@]}"; do echo "    - $f"; done
			echo
		fi

		echo "  Bypass (only if you are certain dist is in sync): LEMA_SKIP_EXTRACT_CHECK=1 bash scripts/publish.sh"
	} >&2
	exit 1
fi

echo "drift check: OK — extracted Go source matches the monorepo"
