package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// TestCheckApproachSettledVerdict encodes the ADR-0110 fold-in: when /fuse returns
// the affirmative `settled` verdict (the corpus holds the in-force accepted choice),
// check_approach surfaces it as a grounded fire — the cited decisions, the grounding
// steer, and the honest absent-capability caveats — and does NOT attach the abstain
// upgrade CTA (settled is not an abstain). This is the behavior that lets `settled`
// and `why_not_public` retire with zero capability loss.
func TestCheckApproachSettledVerdict(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		bc := 0.84
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "react-rfcs", "approach": "use the new transitions API",
			"verdict": "settled",
			"sources": []any{
				map[string]any{
					"n": 1, "ref": "ADR-0042", "type": "chosen",
					"text": "we adopt concurrent transitions via startTransition",
					"binding_cosine": bc,
				},
			},
			"how":  map[string]any{"doc_home": "https://react.dev", "topic": "we adopt concurrent transitions via startTransition"},
			"note": "this is the recorded, in-force choice",
		})
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "use the new transitions API")
	if err != nil {
		t.Fatalf("runCheckApproach: %v", err)
	}
	if out.Verdict != "settled" {
		t.Fatalf("verdict = %q, want settled", out.Verdict)
	}
	if len(out.Sources) != 1 || out.Sources[0].Ref != "ADR-0042" {
		t.Fatalf("settled must cite the governing decision, got %+v", out.Sources)
	}
	// Grounded fire → relay-as-record steer + honest caveats ride along.
	if out.GroundingNote == "" {
		t.Error("settled is a grounded fire and must carry the grounding note")
	}
	if len(out.Caveats) == 0 {
		t.Error("settled must carry the absent-capability caveats (no decision graph / relitigation / dates)")
	}
	// settled is NOT an abstain → no connect-your-repo upgrade CTA.
	if out.Upgrade != "" {
		t.Errorf("settled must NOT carry the abstain upgrade CTA: %q", out.Upgrade)
	}
	// The HOW topic points at the sanctioned approach itself (the chosen text), not empty.
	if out.How.Topic == "" {
		t.Error("settled should carry the how topic (the governing chosen approach)")
	}
}

// TestCheckApproachNoRulingStillAbstains pins that the default branch is unchanged
// by the new settled case: a no_recorded_ruling verdict still attaches the honest
// upgrade CTA and carries no grounding note.
func TestCheckApproachNoRulingStillAbstains(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "react-rfcs", "approach": "x",
			"verdict": "no_recorded_ruling",
			"sources": []any{},
			"how":     map[string]any{"doc_home": "https://react.dev"},
			"note":    "no recorded ruling matched",
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
	if out.Verdict != "no_recorded_ruling" {
		t.Fatalf("verdict = %q, want no_recorded_ruling", out.Verdict)
	}
	if out.Upgrade == "" {
		t.Error("no_recorded_ruling abstain must carry the upgrade CTA")
	}
	if out.GroundingNote != "" {
		t.Errorf("an abstain must not carry the grounding note: %q", out.GroundingNote)
	}
}
