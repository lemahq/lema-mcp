package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// recallWhyStub answers /fuse with the ADR-0121 recall-WHY shape: no governing
// ruling fired, but retrieval grounded recorded reasoning, so the backend
// synthesizes a `why` and serves it alongside the honest no_recorded_ruling
// verdict (fuse.go writeFuseRecallWhy). The web /fuse front door already serves
// this; these tests pin how the MCP check_approach tool relays it.
func recallWhyStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "react-rfcs", "approach": "how does reconciliation work",
			"verdict": "no_recorded_ruling",
			"why":     "React reconciles via a heuristic virtual-DOM diff [1].",
			"sources": []any{map[string]any{
				"n": 1, "ref": "RFC-0001", "type": "accepted",
				"text":           "reconciliation uses a heuristic O(n) diff",
				"url":            "https://github.com/facebook/react/issues/2",
				"binding_cosine": 0.7,
			}},
			"how":  map[string]any{"doc_home": "https://react.dev"},
			"note": "",
		})
	}))
}

// TestCheckApproachRecallWhySurfacedByDefault pins the recall-WHY carrier fold
// (ADR-0121, the why_decided fold code-half), now DEFAULT-ON (the seeded-corpus
// eval cleared — 0 hallucinations, 7/8 grounded — so the gate has no remaining
// job): with NO env set, the synthesized `why` the backend already serves reaches
// the agent through check_approach — but it is recorded reasoning, NOT a ruling,
// so the verdict stays no_recorded_ruling, coverage reports insufficient with the
// recall note, and the abstain CTA still rides (matched reasoning never reads as
// clearance).
func TestCheckApproachRecallWhySurfacedByDefault(t *testing.T) {
	ts := recallWhyStub(t)
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "how does reconciliation work")
	if err != nil {
		t.Fatalf("runCheckApproach: %v", err)
	}
	if out.Verdict != "no_recorded_ruling" {
		t.Fatalf("verdict = %q, want no_recorded_ruling (recall-WHY is never a ruling)", out.Verdict)
	}
	if out.Why == "" {
		t.Fatal("recall-WHY `why` must be surfaced by default (the eval gate cleared; the flag is gone)")
	}
	if out.Coverage == nil || out.Coverage.Sufficient {
		t.Errorf("recall-WHY must report insufficient coverage (reasoning, not clearance), got %+v", out.Coverage)
	}
	if out.Coverage.Note != coverageRecallNote {
		t.Errorf("recall-WHY coverage note = %q, want the recall note", out.Coverage.Note)
	}
	if out.Upgrade == "" {
		t.Error("a recall-WHY abstain still carries the connect-your-repo CTA — it is not clearance")
	}
}
