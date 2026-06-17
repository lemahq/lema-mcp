package main

import (
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
	"github.com/lemahq/lema-mcp/internal/verdict"
)

// check_decided must return the typed verdict envelope (ADR-0094) ALONGSIDE the
// legacy topic/decided/closed fields, so the published lema-mcp clients keep
// working while new callers can read the verdict.
func TestCheckOutput_AdditiveEnvelope(t *testing.T) {
	merged := []source.Atom{
		{MatchKey: "redis cache", Closed: true, Type: "rejected_alternative", Ref: "ADR-12", ClosedNote: "do not propose redis cache"},
	}
	out := buildCheckOutput("should we add a redis cache", merged)

	// legacy fields still populated (back-compat)
	if out.Topic != "should we add a redis cache" {
		t.Fatalf("topic must round-trip, got %q", out.Topic)
	}
	if !out.Decided || len(out.Closed) == 0 {
		t.Fatalf("legacy decided/closed must still populate; decided=%v closed=%d", out.Decided, len(out.Closed))
	}

	// new envelope
	if out.Verdict != string(verdict.RuledOut) {
		t.Fatalf("want verdict %q, got %q (reason: %s)", verdict.RuledOut, out.Verdict, out.Reason)
	}
	if len(out.GoverningDecisions) == 0 {
		t.Fatal("ruled_out must carry governing_decisions")
	}
	if out.GoverningDecisions[0].DerivedForce != verdict.Binding {
		t.Errorf("a rejected_alternative of a live decision must be binding, got %q", out.GoverningDecisions[0].DerivedForce)
	}
	if out.Reason == "" {
		t.Error("verdict must carry a reason")
	}
}

func TestCheckOutput_NoMatchIsNotRuledOut(t *testing.T) {
	merged := []source.Atom{
		{MatchKey: "redis cache", Closed: true, Type: "rejected_alternative"},
	}
	out := buildCheckOutput("pick a logging library", merged)
	if out.Verdict != string(verdict.NotRuledOut) {
		t.Fatalf("want not_ruled_out on no match, got %q", out.Verdict)
	}
	if out.Decided || len(out.Closed) != 0 {
		t.Fatalf("no match → decided=false, closed empty; got decided=%v closed=%d", out.Decided, len(out.Closed))
	}
}
