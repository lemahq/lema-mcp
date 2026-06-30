package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHostedCheckApproachRuledOut pins the #293 hosted client leg: CheckApproach
// POSTs {approach, workspace_ids} to the authed POST /check-approach with the bearer
// token, and decodes the same fuse verdict shape the public Fuse path returns — so
// the MCP tool can map an own-corpus ruled_out exactly like a commons one.
func TestHostedCheckApproachRuledOut(t *testing.T) {
	var gotAuth, gotPath, gotApproach string
	var gotWS []any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotApproach, _ = req["approach"].(string)
		gotWS, _ = req["workspace_ids"].([]any)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "ws-acme", "approach": "add two-way data binding", "verdict": "ruled_out",
			"why_not": "the team rejected two-way data binding [1].",
			"sources": []map[string]any{{"n": 1, "ref": "ADR-0001", "type": "rejected", "text": "two-way binding rejected"}},
			"note":    "this approach touches a recorded rejection",
		})
	}))
	defer ts.Close()

	h := NewHosted(ts.URL, "lema_live_x", ts.Client())
	res, err := h.CheckApproach(context.Background(), "add two-way data binding", []string{"ws-1", "ws-2"})
	if err != nil {
		t.Fatalf("CheckApproach: %v", err)
	}
	if gotAuth != "Bearer lema_live_x" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Bearer lema_live_x")
	}
	if gotPath != "/check-approach" {
		t.Errorf("path = %q, want /check-approach", gotPath)
	}
	if gotApproach != "add two-way data binding" {
		t.Errorf("forwarded approach = %q", gotApproach)
	}
	if len(gotWS) != 2 {
		t.Errorf("forwarded workspace_ids = %v, want 2", gotWS)
	}
	if res.Verdict != "ruled_out" {
		t.Fatalf("verdict = %q, want ruled_out", res.Verdict)
	}
	if res.WhyNot == "" || len(res.Sources) != 1 || res.Sources[0].Ref != "ADR-0001" {
		t.Errorf("ruled_out lost its cited why-not: %+v", res)
	}
}

// TestHostedCheckApproachOmitsEmptyWorkspaceIDs pins that an org-wide check (no
// focus) sends no workspace_ids key, so the server resolves the caller's full scope
// rather than an empty list.
func TestHostedCheckApproachOmitsEmptyWorkspaceIDs(t *testing.T) {
	var sawKey bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, sawKey = raw["workspace_ids"]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "all", "approach": "x", "verdict": "no_recorded_ruling", "sources": []any{},
		})
	}))
	defer ts.Close()

	h := NewHosted(ts.URL, "tok", ts.Client())
	if _, err := h.CheckApproach(context.Background(), "x", nil); err != nil {
		t.Fatalf("CheckApproach: %v", err)
	}
	if sawKey {
		t.Error("empty workspace_ids must be omitted from the request body")
	}
}
