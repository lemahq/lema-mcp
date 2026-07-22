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

// TestCheckOutput_LexicalButNotStructural pins the output reconciliation: several
// options share a recurring category term (pipeline) but each carries its own
// identity; a topic naming only the category term REACHES them lexically (Match
// fires) but governs none. The legacy decided/closed/note must mirror the structural
// verdict (verdict.Governing), not the raw Match — otherwise the output says
// "decided:true, do not re-propose" over vocabulary-collision atoms while the verdict
// reads not_ruled_out. That contradiction over 53 atoms was the reported bug.
func TestCheckOutput_LexicalButNotStructural(t *testing.T) {
	merged := []source.Atom{
		{MatchKey: "identone pipeline schema", Closed: true, Type: "rejected_alternative", Ref: "a1"},
		{MatchKey: "identtwo pipeline worker", Closed: true, Type: "rejected_alternative", Ref: "a2"},
		{MatchKey: "identthree pipeline queue", Closed: true, Type: "rejected_alternative", Ref: "a3"},
		{MatchKey: "identfour adapter ledger", Closed: true, Type: "rejected_alternative", Ref: "a4"},
		{MatchKey: "identfive boundary beacon", Closed: true, Type: "rejected_alternative", Ref: "a5"},
		{MatchKey: "identsix registry session", Closed: true, Type: "rejected_alternative", Ref: "a6"},
		{MatchKey: "identseven token payload", Closed: true, Type: "rejected_alternative", Ref: "a7"},
		{MatchKey: "identeight envelope snapshot", Closed: true, Type: "rejected_alternative", Ref: "a8"},
	}
	// "pipeline" is in 3/8 atoms — rare enough that Match fires, common enough (df 3 >
	// distinctiveDF 2) that it is not an identity term.
	out := buildCheckOutput("rework the pipeline flow across services", merged)
	if out.Verdict != string(verdict.NotRuledOut) {
		t.Fatalf("vocabulary-only overlap must be not_ruled_out, got %q (reason %s)", out.Verdict, out.Reason)
	}
	if out.Decided || len(out.Closed) != 0 || out.Note != "" {
		t.Fatalf("legacy fields must mirror the gate, not raw Match: decided=%v closed=%d note=%q", out.Decided, len(out.Closed), out.Note)
	}
}
