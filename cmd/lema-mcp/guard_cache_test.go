package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// F8's contract: the guard enforces the LIVE hosted record through a local
// cache, with zero network on the per-edit path and zero new guard semantics.
// These tests pin the refresh (scoped, atomic, fail-open) and the read side
// (cached atoms fire through the same matcher local captures do).

// closedAtomsServer serves the two hosted routes the refresh touches:
// /workspaces (slug→UUID resolution, #27's path) and /closed-atoms. It
// records the workspace_ids scope the fetch asked for.
func closedAtomsServer(t *testing.T, workspaces []map[string]string, gotScope *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": workspaces})
		case "/closed-atoms":
			*gotScope = r.URL.Query()["workspace_ids"]
			_ = json.NewEncoder(w).Encode(map[string]any{
				"atoms": []map[string]any{{
					"id": "c1", "type": "rejected_alternative",
					"text":        "Kafka — rejected: ops burden",
					"ref":         "ADR-0012",
					"locator":     "lemahq/lema#34",
					"closed":      true,
					"closed_note": `do not propose "Kafka": ops burden (ADR-0012 · "Event transport")`,
					"match_key":   "Kafka",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

// isolateHostedEnv points HOME at a temp dir (so the developer's real
// ~/.config/lema/credentials can never leak into a test) and clears the
// hosted env vars; callers then set what the case needs.
func isolateHostedEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")
	t.Setenv("LEMA_WORKSPACE_ID", "")
}

func TestGuardRefreshWritesCacheScopedToResolvedPin(t *testing.T) {
	isolateHostedEnv(t)
	const uuid = "b691e8ae-1111-4222-8333-444455556666"
	var gotScope []string
	ts := closedAtomsServer(t, []map[string]string{{"id": uuid, "slug": "lemahq-lema"}}, &gotScope)
	defer ts.Close()
	t.Setenv("LEMA_API_URL", ts.URL)
	t.Setenv("LEMA_API_TOKEN", "lema_live_tok")
	t.Setenv("LEMA_WORKSPACE_ID", "lemahq-lema") // a SLUG — must resolve, not pass through

	capturePath := filepath.Join(t.TempDir(), ".lema", "decisions.jsonl")
	runGuardRefresh(capturePath)

	if len(gotScope) != 1 || gotScope[0] != uuid {
		t.Fatalf("fetch must be scoped to the slug's resolved UUID, got %v", gotScope)
	}
	data, err := os.ReadFile(guardCacheFile(capturePath))
	if err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	var c guardCache
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("cache is not valid JSON: %v", err)
	}
	if len(c.Atoms) != 1 || c.Atoms[0].MatchKey != "Kafka" || !c.Atoms[0].Closed {
		t.Fatalf("cache must carry the hosted closed atoms verbatim: %+v", c.Atoms)
	}
	if c.FetchedAt == "" || len(c.WorkspaceIDs) != 1 || c.WorkspaceIDs[0] != uuid {
		t.Fatalf("cache must record fetched_at + the resolved scope: %+v", c)
	}
}

// An unresolvable pin (workspace not visible to the credential) falls back to
// the unscoped fetch — caching everything visible beats caching nothing.
func TestGuardRefreshUnresolvablePinFallsBackUnscoped(t *testing.T) {
	isolateHostedEnv(t)
	var gotScope []string
	ts := closedAtomsServer(t, []map[string]string{}, &gotScope)
	defer ts.Close()
	t.Setenv("LEMA_API_URL", ts.URL)
	t.Setenv("LEMA_API_TOKEN", "lema_live_tok")
	t.Setenv("LEMA_WORKSPACE_ID", "not-a-visible-workspace")

	capturePath := filepath.Join(t.TempDir(), ".lema", "decisions.jsonl")
	runGuardRefresh(capturePath)

	if len(gotScope) != 0 {
		t.Fatalf("unresolvable pin must fall back to an unscoped fetch, got scope %v", gotScope)
	}
	if _, err := os.Stat(guardCacheFile(capturePath)); err != nil {
		t.Fatalf("cache should still be written from the unscoped fetch: %v", err)
	}
}

// Solo tier: no hosted config → the refresh is a silent no-op, never an error
// and never an empty cache file that could mask a previously good one.
func TestGuardRefreshNoHostedConfigIsNoop(t *testing.T) {
	isolateHostedEnv(t)
	capturePath := filepath.Join(t.TempDir(), ".lema", "decisions.jsonl")
	runGuardRefresh(capturePath)
	if _, err := os.Stat(guardCacheFile(capturePath)); !os.IsNotExist(err) {
		t.Fatalf("no hosted config must write no cache, stat err = %v", err)
	}
}

// The read side: a cached hosted closure fires through the SAME matcher and
// mode contract a locally captured closure does — the no-new-semantics pin.
func TestGuardFiresFromCachedHostedAtom(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), ".lema", "decisions.jsonl")
	c := guardCache{
		FetchedAt: "2026-07-21T21:00:00Z",
		Atoms: []guardCacheAtom{{
			ID: "c1", Type: "rejected_alternative",
			Text:       "Kafka — rejected: ops burden",
			Ref:        "ADR-0012",
			Closed:     true,
			ClosedNote: `do not propose "Kafka": ops burden (ADR-0012 · "Event transport")`,
			MatchKey:   "Kafka",
		}},
	}
	data, _ := json.Marshal(c)
	if err := os.MkdirAll(filepath.Dir(guardCacheFile(capturePath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guardCacheFile(capturePath), data, 0o600); err != nil {
		t.Fatal(err)
	}

	closed := loadGuardCacheAtoms(capturePath)
	out, atom := evaluateGuard(closed, ctxQuery("queue.go", "kafka.NewProducer()"), guardModeContext)
	if out == nil || atom == nil || atom.MatchKey != "Kafka" {
		t.Fatalf("a cached hosted closure must fire like a local one: out=%v atom=%+v", out, atom)
	}
	// And an unrelated edit stays silent — the cache adds coverage, not noise.
	if out, _ := evaluateGuard(closed, ctxQuery("x.go", "reduce operational burden"), guardModeContext); out != nil {
		t.Fatalf("no distinctive match must stay silent: %+v", out)
	}
}

// Fail-open on the read side: missing or corrupt cache contributes nothing
// and never errors.
func TestGuardCacheMissingOrCorruptFailsOpen(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), ".lema", "decisions.jsonl")
	if atoms := loadGuardCacheAtoms(capturePath); atoms != nil {
		t.Fatalf("missing cache must yield nil, got %+v", atoms)
	}
	if err := os.MkdirAll(filepath.Dir(guardCacheFile(capturePath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guardCacheFile(capturePath), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if atoms := loadGuardCacheAtoms(capturePath); atoms != nil {
		t.Fatalf("corrupt cache must yield nil, got %+v", atoms)
	}
}
