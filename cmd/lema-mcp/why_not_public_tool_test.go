package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

func TestWhyNotPublicAppliesRuledOutTemplate(t *testing.T) {
	var gotAuth, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotQuery, _ = req["query"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scope": "react-rfcs", "answer": "Yes — mixins were ruled out in favor of Hooks [1].",
			"sources": []map[string]any{{
				"n": 1, "ref": "reactjs/rfcs#68", "type": "rejected", "text": "mixins ruled out",
				"status": "accepted", "rejected_alternatives": []string{"mixins"}, "relevance": 0.82,
			}},
			"usage": map[string]any{"atoms_tokens": 150, "source_tokens": 3000, "compression_ratio": 20.0},
		})
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	_, out, err := whyNotPublic(context.Background(), nil, whyNotPublicInput{Repo: "react", Option: "mixins"})
	if err != nil {
		t.Fatalf("whyNotPublic: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("must send no Authorization header, got %q", gotAuth)
	}
	if !strings.Contains(gotQuery, "mixins") {
		t.Errorf("query must contain the option; got %q", gotQuery)
	}
	low := strings.ToLower(gotQuery)
	if !strings.Contains(low, "ruled out") && !strings.Contains(low, "rejected") && !strings.Contains(low, "discouraged") {
		t.Errorf("query must be framed as a ruled-out check; got %q", gotQuery)
	}
	if len(out.Sources) != 1 || !strings.Contains(out.Sources[0].Receipt, "ruled out: mixins") {
		t.Errorf("expected the cited ruled-out receipt; got %+v", out.Sources)
	}
}

func TestWhyNotPublicUnknownRepo(t *testing.T) {
	prev := publicSrc
	publicSrc = source.NewPublic("http://unused.invalid", nil)
	defer func() { publicSrc = prev }()
	if _, _, err := whyNotPublic(context.Background(), nil, whyNotPublicInput{Repo: "django", Option: "x"}); err == nil {
		t.Fatalf("expected error for unknown repo")
	}
}
