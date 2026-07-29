package verdict

import (
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// oldSolo replicates the PRE-fix solo clause so the recall delta can be measured
// on the real corpus instead of argued. Old rule: cov over DISTINCTIVE terms only.
func oldSolo(query string, a source.Atom, df map[string]int, n int) bool {
	q := map[string]bool{}
	for _, t := range gateTerms(query) {
		q[t] = true
	}
	dd := distinctiveDF(n)
	atomDistinct, shared := 0, 0
	for _, t := range gateTerms(matchKeyFor(a)) {
		if df[t] > dd {
			continue
		}
		atomDistinct++
		if q[t] {
			shared++
		}
	}
	if atomDistinct == 0 || shared == 0 {
		return false
	}
	cov := float64(shared) / float64(atomDistinct)
	return (shared >= 2 && cov >= structuralCov) || cov >= structuralCovSolo
}

// The recall question the verbatim self-recall test cannot see: a NATURAL
// relitigation names an option's distinctive identity in a sentence of its own
// words, not the option's full key. For every atom, query = its distinctive terms
// in neutral filler. Reports which atoms the tightened solo bar stops firing on,
// so the precision/recall trade is a measured number, not a claim.
func TestSoloGate_RecallDeltaOnRealCorpus(t *testing.T) {
	atoms := loadRealCorpus(t)
	df, n := OptionDF(atoms)
	dd := distinctiveDF(n)

	naturalQuery := func(a source.Atom) string {
		var d []string
		for _, tk := range gateTerms(matchKeyFor(a)) {
			if df[tk] <= dd {
				d = append(d, tk)
			}
		}
		if len(d) == 0 {
			return ""
		}
		return "let us reconsider " + strings.Join(d, " ") + " for this work"
	}

	oldFire, newFire, lost := 0, 0, 0
	var examples []string
	for _, a := range atoms {
		qs := naturalQuery(a)
		if qs == "" {
			continue
		}
		o := oldSolo(qs, a, df, n)
		w := structuralMatch(qs, a, df, n)
		if o {
			oldFire++
		}
		if w {
			newFire++
		}
		if o && !w {
			lost++
			if len(examples) < 12 {
				examples = append(examples, matchKeyFor(a))
			}
		}
	}
	t.Logf("natural relitigation, %d atoms: old fires=%d  new fires=%d  LOST=%d (%.1f%%)",
		len(atoms), oldFire, newFire, lost, 100*float64(lost)/float64(oldFire))
	for _, e := range examples {
		t.Logf("   no longer self-fires on its own distinctive terms: %q", e)
	}
}
