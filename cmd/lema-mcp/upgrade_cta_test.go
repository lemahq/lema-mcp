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

// TestPublicAskAbstainAttachesUpgradeCTA: on a real abstain (200 with empty
// sources), the output carries the honest connect-your-repo nudge — never a
// paywall, always attributed.
func TestPublicAskAbstainAttachesUpgradeCTA(t *testing.T) {
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
	if len(out.Sources) != 0 {
		t.Fatalf("expected abstain (no sources), got %d", len(out.Sources))
	}
	if out.Upgrade == "" {
		t.Fatal("abstain must attach an upgrade CTA")
	}
	if !strings.Contains(strings.ToLower(out.Upgrade), "connect") || !strings.Contains(out.Upgrade, "utm_source=lema-mcp") {
		t.Errorf("CTA must invite connecting the repo, with attribution: %q", out.Upgrade)
	}
	// Honesty: never a paywall-on-refusal, never imply a withheld answer.
	for _, bad := range []string{"pay", "unlock", "withheld", "upgrade to see", "purchase"} {
		if strings.Contains(strings.ToLower(out.Upgrade), bad) {
			t.Errorf("CTA must not read as paywall-on-refusal (found %q): %q", bad, out.Upgrade)
		}
	}
}

// TestWhyNotPublicAbstainAlsoGetsCTA: the shared runPublicQuery path means
// why_not_public abstains carry the same nudge.
func TestWhyNotPublicAbstainAlsoGetsCTA(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scope": "react-rfcs", "answer": "No recorded decision against it.",
			"sources": []any{}, "usage": map[string]any{},
		})
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	_, out, err := whyNotPublic(context.Background(), nil, whyNotPublicInput{Repo: "react", Option: "x"})
	if err != nil {
		t.Fatalf("whyNotPublic: %v", err)
	}
	if out.Upgrade == "" {
		t.Error("why_not_public abstain should also carry the upgrade CTA")
	}
}

// TestPublicAskGroundedHasNoUpgradeCTA: a grounded (cited) answer must NOT carry
// the CTA — the upsell only fires when the corpus genuinely had nothing.
func TestPublicAskGroundedHasNoUpgradeCTA(t *testing.T) {
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
	if out.Upgrade != "" {
		t.Errorf("grounded answer must NOT carry the upgrade CTA: %q", out.Upgrade)
	}
}

// TestPublicAsk404DegradeHasNoUpgradeCTA: the "graph isn't loaded yet" degrade is
// an infra state, not an abstain — it must not fire the connect-your-repo nudge.
func TestPublicAsk404DegradeHasNoUpgradeCTA(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	_, out, err := publicAsk(context.Background(), nil, publicAskInput{Repo: "rust", Query: "x"})
	if err != nil {
		t.Fatalf("publicAsk: %v", err)
	}
	if out.Upgrade != "" {
		t.Errorf("404 'not loaded' degrade must not carry the upgrade CTA: %q", out.Upgrade)
	}
}
