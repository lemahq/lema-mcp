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
