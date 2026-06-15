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

func TestPublicAskCitesAndAttachesReceipts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("public_ask must send no Authorization header")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scope": "react-rfcs", "answer": "Mixins were rejected for Hooks [1].",
			"sources": []map[string]any{{
				"n": 1, "ref": "reactjs/rfcs#68", "type": "rejected", "text": "mixins ruled out",
				"status": "accepted", "rejected_alternatives": []string{"mixins"}, "relevance": 0.8,
			}},
			"usage": map[string]any{"atoms_tokens": 180, "source_tokens": 3400, "compression_ratio": 18.9},
		})
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	_, out, err := publicAsk(context.Background(), nil, publicAskInput{Repo: "react", Query: "why not mixins?"})
	if err != nil {
		t.Fatalf("publicAsk: %v", err)
	}
	if len(out.Sources) != 1 || out.Sources[0].Ref != "reactjs/rfcs#68" {
		t.Fatalf("sources = %+v", out.Sources)
	}
	if !strings.Contains(out.Sources[0].Receipt, "ruled out: mixins") {
		t.Errorf("receipt missing ruled-out: %q", out.Sources[0].Receipt)
	}
	if out.ROINote == "" {
		t.Errorf("grounded answer must carry a roi_note")
	}
}

func TestPublicAskHonestDegradeOn404(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	_, out, err := publicAsk(context.Background(), nil, publicAskInput{Repo: "rust", Query: "q"})
	if err != nil {
		t.Fatalf("publicAsk should degrade, not error: %v", err)
	}
	if !strings.Contains(out.Answer, "isn't loaded yet") || len(out.Sources) != 0 {
		t.Errorf("expected honest not-loaded degradation, got: %+v", out)
	}
	if out.Sources == nil {
		t.Error("degrade must return a non-nil empty sources slice (serializes as [] not null; JS client .filter()s it)")
	}
}

func TestPublicAskUnknownRepo(t *testing.T) {
	prev := publicSrc
	publicSrc = source.NewPublic("http://unused.invalid", nil)
	defer func() { publicSrc = prev }()
	if _, _, err := publicAsk(context.Background(), nil, publicAskInput{Repo: "django", Query: "q"}); err == nil {
		t.Fatalf("expected error for unknown repo")
	}
}

// TestPublicAskGroundedCarriesGroundingNote: a grounded (cited) answer must carry
// the grounding note — the synthesis-time steer that keeps the consuming agent
// from folding its own model recall in among the real [n] citations under a "from
// the record" banner. This is the honesty boundary the note exists to hold; if it
// silently drops, agents re-blur grounded-vs-recall and overstate the trust claim.
func TestPublicAskGroundedCarriesGroundingNote(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scope": "react-rfcs", "answer": "Mixins were rejected [1].",
			"sources": []map[string]any{{"n": 1, "ref": "reactjs/rfcs#68", "type": "rejected", "text": "x"}},
			"usage":   map[string]any{"atoms_tokens": 10, "source_tokens": 100, "compression_ratio": 10.0},
		})
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	_, out, err := publicAsk(context.Background(), nil, publicAskInput{Repo: "react", Query: "x"})
	if err != nil {
		t.Fatalf("publicAsk: %v", err)
	}
	if out.GroundingNote == "" {
		t.Fatal("grounded answer must carry a grounding_note (keep the cited record distinct from model recall)")
	}
	low := strings.ToLower(out.GroundingNote)
	if !strings.Contains(low, "record") || !strings.Contains(low, "your own") {
		t.Errorf("grounding note must steer 'cited = record, keep your own knowledge separate': %q", out.GroundingNote)
	}
}

// TestPublicAskAbstainHasNoGroundingNote: on an abstain there is no cited record to
// be distinct from, so the note would be noise (and waste output tokens) — it must
// be empty, mirroring roi_note. The abstain already carries the upgrade CTA.
func TestPublicAskAbstainHasNoGroundingNote(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scope": "react-rfcs", "answer": "No recorded decision matched.",
			"sources": []any{}, "usage": map[string]any{},
		})
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	_, out, err := publicAsk(context.Background(), nil, publicAskInput{Repo: "react", Query: "x"})
	if err != nil {
		t.Fatalf("publicAsk: %v", err)
	}
	if out.GroundingNote != "" {
		t.Errorf("abstain must NOT carry a grounding note: %q", out.GroundingNote)
	}
}
