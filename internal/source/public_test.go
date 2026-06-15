package source

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPublicAskSendsNoAuthAndSlug is the one thing a copy-paste from Hosted.Ask
// gets wrong: the public path must send NO Authorization header and a {slug,query}
// body to /ask-public, and map the widened source fields through.
func TestPublicAskSendsNoAuthAndSlug(t *testing.T) {
	var gotAuth, gotPath, gotSlug, gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotSlug, _ = req["slug"].(string)
		gotQuery, _ = req["query"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scope":  "react-rfcs",
			"answer": "React rejected mixins for Hooks [1].",
			"sources": []map[string]any{{
				"n": 1, "ref": "reactjs/rfcs#68", "type": "rejected",
				"text": "mixins ruled out", "status": "accepted",
				"rejected_alternatives": []string{"mixins"}, "relevance": 0.77,
				"url": "https://github.com/reactjs/rfcs/pull/68",
			}},
			"usage":               map[string]any{"atoms_tokens": 180, "source_tokens": 3400, "tokens_saved": 3220, "compression_ratio": 18.9},
			"synthesis_tokens_in": 120, "synthesis_tokens_out": 40,
		})
	}))
	defer ts.Close()

	p := NewPublic(ts.URL, ts.Client())
	res, err := p.PublicAsk(context.Background(), "react-rfcs", "why not mixins?")
	if err != nil {
		t.Fatalf("PublicAsk: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (public path sends no bearer)", gotAuth)
	}
	if gotPath != "/ask-public" {
		t.Errorf("path = %q, want /ask-public", gotPath)
	}
	if gotSlug != "react-rfcs" || gotQuery != "why not mixins?" {
		t.Errorf("body = {slug:%q, query:%q}", gotSlug, gotQuery)
	}
	if len(res.Sources) != 1 || res.Sources[0].Ref != "reactjs/rfcs#68" {
		t.Fatalf("sources = %+v", res.Sources)
	}
	if res.Sources[0].Relevance == nil || *res.Sources[0].Relevance != 0.77 {
		t.Errorf("relevance not mapped: %v", res.Sources[0].Relevance)
	}
	if res.Usage.TokensSaved != 3220 {
		t.Errorf("tokens_saved = %d, want 3220", res.Usage.TokensSaved)
	}
}

func TestPublicAskGraphNotLoadedOn404(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	p := NewPublic(ts.URL, ts.Client())
	if _, err := p.PublicAsk(context.Background(), "rust-rfcs", "q"); !errors.Is(err, ErrPublicGraphNotLoaded) {
		t.Fatalf("err = %v, want ErrPublicGraphNotLoaded", err)
	}
}
