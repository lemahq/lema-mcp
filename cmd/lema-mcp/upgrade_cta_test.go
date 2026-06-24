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

// These pin the abstain/grounded/degrade upgrade-CTA paths on check_approach — the
// one surviving public door (ADR-0124). They moved here from the dropped why_decided
// handler (runPublicQuery), which shared the SAME abstainUpgradeCTA const, so the
// coverage rides forward onto runCheckApproach unchanged.

// TestCheckApproachAbstainAttachesUpgradeCTA: on a real abstain (200
// no_recorded_ruling with empty sources), the output carries the honest
// connect-your-repo nudge — never a paywall, always attributed.
func TestCheckApproachAbstainAttachesUpgradeCTA(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "react-rfcs", "approach": "x", "verdict": "no_recorded_ruling",
			"sources": []any{}, "how": map[string]any{"doc_home": "https://react.dev"},
		})
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "x")
	if err != nil {
		t.Fatalf("runCheckApproach: %v", err)
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

// TestCheckApproachGroundedHasNoUpgradeCTA: a grounded (cited) ruled_out must NOT
// carry the CTA — the upsell only fires when the corpus genuinely had nothing.
func TestCheckApproachGroundedHasNoUpgradeCTA(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "react-rfcs", "approach": "mixins", "verdict": "ruled_out",
			"why_not": "Mixins were rejected [1].",
			"sources": []map[string]any{{"n": 1, "ref": "reactjs/rfcs#68", "type": "rejected", "text": "x"}},
			"how":     map[string]any{"doc_home": "https://react.dev"},
		})
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "mixins")
	if err != nil {
		t.Fatalf("runCheckApproach: %v", err)
	}
	if out.Verdict != "ruled_out" {
		t.Fatalf("verdict = %q, want ruled_out (grounded)", out.Verdict)
	}
	if out.Upgrade != "" {
		t.Errorf("grounded answer must NOT carry the upgrade CTA: %q", out.Upgrade)
	}
}

// TestCheckApproach404DegradeHasNoUpgradeCTA: the "graph isn't loaded yet" degrade
// is an infra state, not an abstain — it must not fire the connect-your-repo nudge.
func TestCheckApproach404DegradeHasNoUpgradeCTA(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	out, err := runCheckApproach(context.Background(), "check_approach", "rust", "x")
	if err != nil {
		t.Fatalf("runCheckApproach: %v", err)
	}
	if out.Upgrade != "" {
		t.Errorf("404 'not loaded' degrade must not carry the upgrade CTA: %q", out.Upgrade)
	}
}
