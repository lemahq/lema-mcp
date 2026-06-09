package source

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostedSearchMapsAtoms(t *testing.T) {
	var gotAuth, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotQuery, _ = req["query"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"atoms": []map[string]string{
				{"type": "chosen", "ref": "ADR-0025", "text": "atom-first storage"},
				{"type": "constraint", "ref": "ADR-0016", "text": "five workers on pub/sub"},
			},
		})
	}))
	defer ts.Close()

	h := NewHosted(ts.URL, "tok123", ts.Client())
	atoms, err := h.Search(context.Background(), "why atoms?", 8)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Bearer tok123")
	}
	if gotQuery != "why atoms?" {
		t.Errorf("forwarded query = %q, want %q", gotQuery, "why atoms?")
	}
	if len(atoms) != 2 || atoms[0].Ref != "ADR-0025" || atoms[1].Type != "constraint" {
		t.Fatalf("atoms mapped wrong: %+v", atoms)
	}
}

// TestHostedSearchMapsLocator pins ADR-0056's hosted decode/map path: a served
// atom carrying a followable "locator" lands on source.Atom.Locator, and one
// without it yields "" — so the agent surface gets the followable provenance
// only when the hosted backend provides it (local-parse atoms stay empty).
func TestHostedSearchMapsLocator(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"atoms": []map[string]any{
				{"type": "chosen", "ref": "owner/repo#123", "text": "PR-backed atom", "locator": "owner/repo#123"},
				{"type": "constraint", "ref": "ADR-0025", "text": "ADR-backed atom"},
			},
		})
	}))
	defer ts.Close()

	h := NewHosted(ts.URL, "tok", ts.Client())
	atoms, err := h.Search(context.Background(), "q", 8)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(atoms) != 2 {
		t.Fatalf("got %d atoms, want 2", len(atoms))
	}
	if atoms[0].Locator != "owner/repo#123" {
		t.Errorf("atoms[0].Locator = %q, want %q", atoms[0].Locator, "owner/repo#123")
	}
	if atoms[1].Locator != "" {
		t.Errorf("atoms[1].Locator = %q, want \"\" (no locator served)", atoms[1].Locator)
	}
}

func TestHostedNonSearchReturnsSentinel(t *testing.T) {
	h := NewHosted("http://example.invalid", "t", nil)
	if _, err := h.List(context.Background(), "", 0); !errors.Is(err, errHostedSearchOnly) {
		t.Errorf("List err = %v, want errHostedSearchOnly", err)
	}
	if _, err := h.Get(context.Background(), 1); !errors.Is(err, errHostedSearchOnly) {
		t.Errorf("Get err = %v, want errHostedSearchOnly", err)
	}
	if _, err := h.Graph(context.Background(), 1, 2); !errors.Is(err, errHostedSearchOnly) {
		t.Errorf("Graph err = %v, want errHostedSearchOnly", err)
	}
}

// TestHostedAskMapsAnswer pins the P3 join (ADR-0059 shape A): Ask POSTs the
// query + workspace focus to /ask with the bearer token, and maps the
// synthesized answer, the cited sources (with their followable locator/url), and
// the token meter — folding the two synthesis legs into one cost number.
func TestHostedAskMapsAnswer(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scope":  "all 2 workspaces",
			"answer": "We chose pgvector [1] over a dedicated store [2].",
			"sources": []map[string]any{
				{"n": 1, "ref": "ADR-0025", "type": "chosen", "text": "pgvector on Postgres"},
				{"n": 2, "ref": "owner/repo#7", "type": "rejected", "text": "Pinecone", "locator": "owner/repo#7", "url": "https://github.com/owner/repo/issues/7"},
			},
			"usage": map[string]any{
				"atoms_tokens": 40, "source_tokens": 3200, "tokens_saved": 3160, "compression_ratio": 80.0,
			},
			"synthesis_tokens_in":  120,
			"synthesis_tokens_out": 35,
		})
	}))
	defer ts.Close()

	h := NewHosted(ts.URL, "tok-xyz", ts.Client())
	res, err := h.Ask(context.Background(), "why pgvector?", []string{"ws-1", "ws-2"})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if gotPath != "/ask" {
		t.Errorf("posted to %q, want /ask", gotPath)
	}
	if gotAuth != "Bearer tok-xyz" {
		t.Errorf("auth = %q, want Bearer tok-xyz", gotAuth)
	}
	if ws, _ := gotBody["workspace_ids"].([]any); len(ws) != 2 {
		t.Errorf("workspace_ids forwarded = %v, want 2", gotBody["workspace_ids"])
	}
	if res.Answer != "We chose pgvector [1] over a dedicated store [2]." {
		t.Errorf("answer = %q", res.Answer)
	}
	if len(res.Sources) != 2 || res.Sources[1].URL != "https://github.com/owner/repo/issues/7" {
		t.Fatalf("sources mapped wrong: %+v", res.Sources)
	}
	if res.Sources[1].Locator != "owner/repo#7" {
		t.Errorf("source locator = %q", res.Sources[1].Locator)
	}
	if res.Usage.CompressionRatio != 80.0 || res.Usage.TokensSaved != 3160 {
		t.Errorf("usage mapped wrong: %+v", res.Usage)
	}
	if res.Usage.SynthesisTokens != 155 {
		t.Errorf("synthesis cost = %d, want 155 (120+35 folded)", res.Usage.SynthesisTokens)
	}
}

func TestHostedAskErrorsOnNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	h := NewHosted(ts.URL, "t", ts.Client())
	if _, err := h.Ask(context.Background(), "q", nil); err == nil {
		t.Error("Ask should error on a non-200 response")
	}
}
