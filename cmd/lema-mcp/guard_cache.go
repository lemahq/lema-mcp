// guard_cache.go is F8's hosted guard read (pivot B2, PIVOT_SPEC §F8): the
// PreToolUse guard enforces the LIVE hosted record, not a frozen snapshot,
// without ever putting a network call on the per-edit path.
//
// The shape is a two-halves cache, refreshed at the session boundary:
//
//   - `lema-mcp guard --refresh-cache` (a SessionStart hook line) fetches the
//     hosted never-reopen feed — the same /closed-atoms read check_decided
//     enforces from — and atomically writes it next to the capture file
//     (.lema/closed.hosted.json). Fail-open and silent: no hosted config, a
//     fetch error, or a timeout leaves the previous cache in place and never
//     blocks session start.
//   - runGuard merges the cached atoms into its closed set alongside the
//     capture store and the repo's prose ADRs. NO new guard semantics
//     (d_8180e2's F8 constraint): same matcher, same fail-open contract,
//     same output — only the closed-set acquisition widens to the hosted
//     record.
//
// Staleness carries the SAME contract the B0 interim snapshot did: closures
// do not expire, so an old cache over-enforces only a since-superseded atom —
// exactly the trade the retired-snapshot wiring accepted at B0, now bounded
// by a session-start refresh instead of frozen at 2026-07-20. fetched_at is
// stored for observability, never used to reject the cache.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
)

// guardCacheRefreshTimeout bounds the session-start fetch. Generous next to
// the per-edit path (which never touches the network): a cold Cloud Run start
// can take seconds, and SessionStart tolerates that once per session.
const guardCacheRefreshTimeout = 10 * time.Second

// guardCache is the on-disk shape of the hosted closed-set cache.
type guardCache struct {
	FetchedAt    string           `json:"fetched_at"`
	APIURL       string           `json:"api_url"`
	WorkspaceIDs []string         `json:"workspace_ids,omitempty"`
	Atoms        []guardCacheAtom `json:"atoms"`
}

// guardCacheAtom is the cache's OWN wire shape for a closed atom.
// source.Atom deliberately excludes MatchKey/MatchKeyDerived from its JSON
// (they are matching aids, not MCP wire fields) — but the matching keys are
// exactly what the guard needs from this cache, so the cache serializes them
// explicitly instead of widening the Atom wire contract.
type guardCacheAtom struct {
	ID              string   `json:"id,omitempty"`
	Type            string   `json:"type,omitempty"`
	Text            string   `json:"text,omitempty"`
	Ref             string   `json:"ref,omitempty"`
	Locator         string   `json:"locator,omitempty"`
	Refs            []string `json:"refs,omitempty"`
	Closed          bool     `json:"closed,omitempty"`
	ClosedNote      string   `json:"closed_note,omitempty"`
	MatchKey        string   `json:"match_key,omitempty"`
	MatchKeyDerived string   `json:"match_key_derived,omitempty"`
}

func toCacheAtom(a source.Atom) guardCacheAtom {
	return guardCacheAtom{
		ID: a.ID, Type: a.Type, Text: a.Text, Ref: a.Ref, Locator: a.Locator,
		Refs: a.Refs, Closed: a.Closed, ClosedNote: a.ClosedNote,
		MatchKey: a.MatchKey, MatchKeyDerived: a.MatchKeyDerived,
	}
}

func (c guardCacheAtom) toAtom() source.Atom {
	return source.Atom{
		ID: c.ID, Type: c.Type, Text: c.Text, Ref: c.Ref, Locator: c.Locator,
		Refs: c.Refs, Closed: c.Closed, ClosedNote: c.ClosedNote,
		MatchKey: c.MatchKey, MatchKeyDerived: c.MatchKeyDerived,
	}
}

// guardCacheFile derives the cache path as a SIBLING of the capture file, so
// it inherits capture_path.go's worktree anchoring for free: every linked
// worktree of a repo shares the main checkout's one cache, exactly like the
// one capture store.
func guardCacheFile(capturePath string) string {
	return filepath.Join(filepath.Dir(capturePath), "closed.hosted.json")
}

// loadGuardCacheAtoms returns the cached hosted closed atoms, or nil on any
// error — a missing, unreadable, or corrupt cache must never block an edit
// (the guard's fail-open contract, ADR-0052).
func loadGuardCacheAtoms(capturePath string) []source.Atom {
	data, err := os.ReadFile(guardCacheFile(capturePath))
	if err != nil {
		return nil
	}
	var c guardCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil
	}
	atoms := make([]source.Atom, len(c.Atoms))
	for i, a := range c.Atoms {
		atoms[i] = a.toAtom()
	}
	return atoms
}

// runGuardRefresh fetches the hosted closed set and atomically replaces the
// cache. Every failure path is silent and leaves any existing cache intact:
// this runs unattended at SessionStart, where a loud error or a truncated
// cache would be worse than serving yesterday's closures. Solo repos (no
// hosted config) exit clean — the guard's capture-store + ADR set already IS
// their whole record.
func runGuardRefresh(capturePath string) {
	apiURL, token, _ := resolveHostedConfig()
	if apiURL == "" || token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), guardCacheRefreshTimeout)
	defer cancel()

	client := &http.Client{Timeout: guardCacheRefreshTimeout}
	// Scope to the pinned workspace when one is configured, resolving a slug
	// pin to its UUID exactly like the push and collector-sync paths (#27) —
	// an unresolvable pin falls back to the unscoped fetch (everything this
	// credential can see) rather than caching nothing.
	var wsIDs []string
	if ws := resolveWorkspaceID(); ws != "" {
		if uuid, err := resolveWorkspaceValueUUID(ctx, client, apiURL, token, ws); err == nil {
			wsIDs = []string{uuid}
		}
	}

	atoms, err := source.NewHosted(apiURL, token, client).FetchClosedAtoms(ctx, wsIDs)
	if err != nil {
		return
	}
	cached := make([]guardCacheAtom, len(atoms))
	for i, a := range atoms {
		cached[i] = toCacheAtom(a)
	}
	c := guardCache{
		FetchedAt:    time.Now().UTC().Format(time.RFC3339),
		APIURL:       apiURL,
		WorkspaceIDs: wsIDs,
		Atoms:        cached,
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	path := guardCacheFile(capturePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	// Atomic replace: a reader (a concurrent PreToolUse guard) sees either the
	// old cache or the new one, never a torn write.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".closed.hosted.*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	fmt.Fprintf(os.Stderr, "lema-mcp: guard cache refreshed — %d hosted closed atom(s)\n", len(atoms))
}
