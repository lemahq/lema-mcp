package verdict

import (
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// Diagnostic for the solo-coverage residual (pain-points #3 residual, #27).
// Measured 2026-07-28 against the live hosted corpus: three unrelated topics
// whose ONLY overlap with a closed atom is the token "MVP" all returned
// ruled_out with derived_force=binding, citing d_c4df27 ("Session-source join
// as MVP path for Judgment pending" — the Judgment pending queue).
//
// This test does not assert; it prints the gate's internals so the mechanism is
// measured rather than reasoned about. Run with LEMA_REAL_CORPUS set.
func TestSoloGateDiagnostic(t *testing.T) {
	atoms := loadRealCorpus(t)
	df, n := OptionDF(atoms)
	dd := distinctiveDF(n)
	t.Logf("corpus n=%d  distinctiveDF cutoff dd=%d (a term is 'identity' if df<=%d)", n, dd, dd)

	topics := []string{
		"Use MVP pricing tiers for enterprise customers",
		"Ship an MVP of the mobile app before the web version",
		"Delete the /decisions/[id]/claim web page as part of the MVP collapse deletion sweep",
	}

	for _, topic := range topics {
		v := Build(atoms, topic)
		t.Logf("\n=== topic: %q\n    verdict=%s governing=%d", topic, v.Verdict, len(v.GoverningDecisions))
		if v.Verdict != RuledOut {
			continue
		}
		for _, a := range Governing(atoms, topic) {
			key := matchKeyFor(a)
			q := map[string]bool{}
			for _, tk := range gateTerms(topic) {
				q[tk] = true
			}
			var kept, dropped []string
			shared := 0
			for _, tk := range gateTerms(key) {
				if df[tk] > dd {
					dropped = append(dropped, tk)
					continue
				}
				kept = append(kept, tk)
				if q[tk] {
					shared++
				}
			}
			cov := 0.0
			if len(kept) > 0 {
				cov = float64(shared) / float64(len(kept))
			}
			t.Logf("    cited atom key: %q", key)
			t.Logf("      dropped as common (df>%d): %v", dd, dropped)
			t.Logf("      kept as identity:          %v", kept)
			t.Logf("      shared=%d atomDistinct=%d cov=%.2f  (solo threshold %.2f)",
				shared, len(kept), cov, structuralCovSolo)
		}
	}
}

// TestSoloGate_DegenerateIdentity pins the DEFECT, not the current behavior: a
// closed atom whose match key is long but whose terms are almost all corpus-common
// collapses to a single "identity" term, and cov = 1/1 = 1.0 clears the solo
// threshold trivially. Coverage of a one-element set is not evidence of identity —
// it is an artifact of the df filter having stripped the key.
//
// Intent: an unrelated topic that shares exactly ONE token with a multi-term option
// must not be ruled_out. This is the direction that stops correct work.
func TestSoloGate_DegenerateIdentityDoesNotGovern(t *testing.T) {
	atoms := loadRealCorpus(t)
	for _, c := range []struct{ name, topic string }{
		{"mvp-pricing", "Use MVP pricing tiers for enterprise customers"},
		{"mvp-mobile", "Ship an MVP of the mobile app before the web version"},
		{"mvp-sweep", "Delete the /decisions/[id]/claim web page as part of the MVP collapse deletion sweep"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if v := Build(atoms, c.topic); v.Verdict == RuledOut {
				var cited string
				if len(v.GoverningDecisions) > 0 {
					cited = v.GoverningDecisions[0].Summary
				}
				t.Errorf("FALSE ruled_out: %q\n  cited: %s", c.topic, cited)
			}
		})
	}
}

var _ = source.Atom{}
