package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// ruledOutStub returns an httptest server that answers /fuse with a `ruled_out`
// verdict carrying n cited rejection atoms, a synthesized why-not, and a HOW that
// names the sanctioned alternative (the what-instead, ADR-0106/0120). n drives the
// coverage of the match; the server only emits ruled_out with n>=1 (fuse.go:617
// abstains when no rejection is cited), so n=0 models a future/buggy upstream.
func ruledOutStub(t *testing.T, n int) *httptest.Server {
	t.Helper()
	bc := 0.74
	sources := make([]any, n)
	for i := 0; i < n; i++ {
		sources[i] = map[string]any{
			"n": i + 1, "ref": fmt.Sprintf("RFC-%04d", 60+i), "type": "rejected",
			"text":           "two-way data binding was rejected for its update-cycle opacity",
			"url":            "https://github.com/facebook/react/issues/1",
			"binding_cosine": bc,
		}
	}
	whyNot := ""
	if n > 0 {
		whyNot = "The team rejected two-way data binding [1]."
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "react-rfcs", "approach": "add two-way data binding",
			"verdict": "ruled_out",
			"why_not": whyNot,
			"sources": sources,
			"how": map[string]any{
				"doc_home": "https://react.dev", "topic": "one-way data flow",
				"sanctioned_alternative": "one-way data flow", "grounding": "corpus_chosen",
			},
			"note": "the project considered and rejected this",
		})
	}))
}

// TestCheckApproachRuledOutLeadsNoAbstainFraming is the envelope-ordering guard
// (codifying #253's TestFuseRulingStillLeadsNoRecallWhy at the MCP layer, ADR-0121):
// a grounded ruled_out is SLICE 1 — the cited why-not is the headline and the
// abstain/recall slices must NOT co-present. Recall-WHY and the connect-your-repo CTA
// are the fallback for SILENCE, never a peer of a ruling; surfacing them alongside a
// ruling would soften a verdict the corpus actually holds.
func TestCheckApproachRuledOutLeadsNoAbstainFraming(t *testing.T) {
	ts := ruledOutStub(t, 2)
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "add two-way data binding")
	if err != nil {
		t.Fatalf("runCheckApproach: %v", err)
	}
	// Slice 1: the ruling leads.
	if out.Verdict != "ruled_out" {
		t.Fatalf("verdict = %q, want ruled_out — a grounded rejection is the headline", out.Verdict)
	}
	if out.WhyNot == "" {
		t.Error("a ruled_out must carry the cited why-not as its headline content")
	}
	if len(out.Sources) == 0 {
		t.Error("a ruling must cite the rejection it rests on")
	}
	// Grounded fire → the relay-as-record steer + honest absent-capability caveats ride.
	if out.GroundingNote == "" {
		t.Error("a grounded ruled_out must carry the grounding note")
	}
	if len(out.Caveats) == 0 {
		t.Error("a grounded ruled_out must carry the absent-capability caveats")
	}
	// The abstain/recall framing must NOT co-present with the ruling:
	if out.Upgrade != "" {
		t.Errorf("a ruling is not an abstain — it must NOT carry the connect-your-repo CTA: %q", out.Upgrade)
	}
	// The coverage slice (slice 4) reports the cited ruling sufficient and NEVER bolts on
	// the "absence is not clearance" recall note — that note belongs to the abstain slice.
	if out.Coverage == nil || !out.Coverage.Sufficient {
		t.Fatalf("a grounded ruling must report sufficient coverage, got %+v", out.Coverage)
	}
	if out.Coverage.Note != "" {
		t.Errorf("a ruling's coverage must not carry the abstain 'not clearance' note: %q", out.Coverage.Note)
	}
}

// TestCheckApproachRuledOutUncitedDemotes pins the envelope-ordering invariant's
// defensive edge, mirroring TestCheckApproachSettledZeroSourcesDemotes: a `ruled_out`
// that arrives with NO cited rejection is not a grounded ruling — the tool must never
// assert the negative verb without the rejection it cites. It must NOT fall through to
// `default` still labelled "ruled_out" (which would emit verdict:"ruled_out" alongside
// a "no matching record" coverage note — a self-contradiction). It demotes to
// no_recorded_ruling with the honest empty-match framing. (Unreachable from today's
// server — fuse.go:617 abstains when len(cited)==0 — this guards a future/buggy upstream.)
func TestCheckApproachRuledOutUncitedDemotes(t *testing.T) {
	ts := ruledOutStub(t, 0)
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "add two-way data binding")
	if err != nil {
		t.Fatalf("runCheckApproach: %v", err)
	}
	if out.Verdict == "ruled_out" {
		t.Fatalf("an uncited ruled_out must NOT remain labelled ruled_out — a rejection verb with no cited rejection is the contradiction this guards")
	}
	if out.Verdict != "no_recorded_ruling" {
		t.Fatalf("verdict = %q, want no_recorded_ruling (the demoted, in-space verb)", out.Verdict)
	}
	// The ungrounded why-not must not pass through as a headline with no citation behind it.
	if out.WhyNot != "" {
		t.Errorf("an uncited demotion must drop the ungrounded why-not, got %q", out.WhyNot)
	}
	// The affirmative HOW (sanctioned alternative / topic) must be stripped — only the plain
	// docs-home pointer survives, same as the settled demotion.
	if out.How.Topic != "" || out.How.SanctionedAlternative != "" {
		t.Errorf("an uncited demotion must STRIP the affirmative how (Topic/SanctionedAlternative), got %+v", out.How)
	}
	if out.How.DocHome == "" {
		t.Error("the demoted output should keep the plain docs-home pointer")
	}
	// Abstain shape: the connect-your-repo CTA and an insufficient, zero-count coverage slice.
	if out.Upgrade == "" {
		t.Error("a demoted (uncited) ruled_out abstains — it must carry the upgrade CTA")
	}
	if out.Coverage == nil || out.Coverage.Sufficient || out.Coverage.MatchedAtoms != 0 {
		t.Errorf("uncited demotion must report 0 matched atoms, insufficient, got %+v", out.Coverage)
	}
	if out.Coverage.Note == "" {
		t.Error("the demoted coverage slice must carry the honest empty-match note")
	}
}
