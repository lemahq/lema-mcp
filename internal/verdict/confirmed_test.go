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

// TestBuildConfirmedSingleDistinctiveTermNeedsStrongerCosine: a lexical hit resting
// on ONE distinctive term (the rest corpus-common) must clear the stronger
// singleTokenTau, not the normal tau — the false-ruled_out guard (a query about TS
// config reaching a ruling about .ts imports shares only the project name + one
// rare word). A match on TWO distinctive terms fires at the normal tau.
func TestBuildConfirmedSingleDistinctiveTermNeedsStrongerCosine(t *testing.T) {
	// "astro" is in every option (corpus-common, low IDF → not distinctive); each
	// tech term is in one (distinctive). Six options so the single-option IDF clears
	// distinctivenessIDF.
	keys := []string{"pinecone store", "redis cache", "kafka bus", "graphql edge", "deno runtime", "turbo monorepo"}
	var closed []source.Atom
	for i, k := range keys {
		closed = append(closed, source.Atom{ID: string(rune('a' + i)), Type: "rejected_alternative", Closed: true, MatchKey: "astro " + k, Ref: "R"})
	}
	mid := (0.725 + singleTokenTau) / 2 // between the two thresholds

	t.Run("single distinctive term, mid cosine → abstain", func(t *testing.T) {
		// shares only "pinecone" (distinctive) + "astro" (common) with option a.
		v := BuildConfirmed(closed, "astro pinecone for the project", map[string]float64{"a": mid}, 0.725)
		if v.Verdict != NotRuledOut {
			t.Errorf("verdict = %q, want not_ruled_out (single distinctive term below singleTokenTau)", v.Verdict)
		}
	})
	t.Run("single distinctive term, high cosine → fire", func(t *testing.T) {
		v := BuildConfirmed(closed, "astro pinecone for the project", map[string]float64{"a": singleTokenTau + 0.02}, 0.725)
		if v.Verdict != RuledOut {
			t.Errorf("verdict = %q, want ruled_out (single distinctive term IS semantically central)", v.Verdict)
		}
	})
	t.Run("two distinctive terms, mid cosine → fire at normal tau", func(t *testing.T) {
		// an option carrying two terms unique to it (df=1 each → both distinctive).
		two := append([]source.Atom{}, closed...)
		two = append(two, source.Atom{ID: "z", Type: "rejected_alternative", Closed: true, MatchKey: "astro mongodb cassandra", Ref: "R"})
		v := BuildConfirmed(two, "astro mongodb cassandra", map[string]float64{"z": mid}, 0.725)
		if v.Verdict != RuledOut {
			t.Errorf("verdict = %q, want ruled_out (two distinctive terms clear normal tau)", v.Verdict)
		}
	})
}
