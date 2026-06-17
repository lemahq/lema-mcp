package verdict

import (
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

func confirmFixture() []source.Atom {
	return []source.Atom{
		{ID: "a1", Type: "rejected_alternative", Closed: true, MatchKey: "make memo the default for function components", Ref: "RFC-1"},
		{ID: "a2", Type: "rejected_alternative", Closed: true, MatchKey: "use a polar layout for complex numbers", Ref: "RFC-2"},
	}
}

func TestBuildConfirmedFiresWhenSemanticallyConfirmed(t *testing.T) {
	closed := confirmFixture()
	sim := map[string]float64{"a1": 0.81, "a2": 0.20}
	v := BuildConfirmed(closed, "make memo default for components", sim, 0.725)
	if v.Verdict != RuledOut {
		t.Fatalf("verdict = %q, want ruled_out", v.Verdict)
	}
	if len(v.GoverningDecisions) != 1 || v.GoverningDecisions[0].Ref != "RFC-1" {
		t.Errorf("governing = %+v, want only RFC-1", v.GoverningDecisions)
	}
}

func TestBuildConfirmedDropsLexicalHitBelowTau(t *testing.T) {
	closed := confirmFixture()
	sim := map[string]float64{"a1": 0.55, "a2": 0.10}
	v := BuildConfirmed(closed, "make memo default for components", sim, 0.725)
	if v.Verdict != NotRuledOut {
		t.Errorf("verdict = %q, want not_ruled_out (lexical hit unconfirmed)", v.Verdict)
	}
	if len(v.GoverningDecisions) != 0 {
		t.Errorf("dropped-all-matches must surface no governing decisions, got %d", len(v.GoverningDecisions))
	}
}

func TestBuildConfirmedNoLexicalMatch(t *testing.T) {
	v := BuildConfirmed(confirmFixture(), "something entirely unrelated xyzzy", map[string]float64{}, 0.725)
	if v.Verdict != NotRuledOut {
		t.Errorf("verdict = %q, want not_ruled_out", v.Verdict)
	}
}
