package verdict

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// This is the structural-gate eval (2026-07-22, decisions a991ec27 + ea22dd9a).
//
// The bug it pins: check_decided returned a false ruled_out because
// Build = buildFrom(Match(...)) skipped a governs-vs-collides test. Every hosted
// atom is Type="rejected_alternative" (Binding), so any lexical overlap clearing
// MatchThreshold fired ruled_out on the first atom — a meta-topic shares this
// corpus's own vocabulary, or a coincidental rare word, with dozens it does not
// govern. The gate requires the query to COVER an atom's distinctive identity.
//
// The CI corpus is SYNTHETIC on purpose: this package syncs into the PUBLIC lema-mcp
// repo, so the real decision corpus cannot be checked in. It is generated to
// reproduce the failure MECHANISM — category vocabulary recurring across many atoms
// (high df, not distinctive) vs unique identity terms (df 1), plus a verbose option
// for the coincidental-sliver case. The gate is a coverage RATIO that transfers
// across corpus sizes by construction, so moderate scale exercises it faithfully.
// The empirical prod-scale precision/recall check runs against the real 500-atom
// binding feed, env-gated below (TestBuildGate_RealCorpus).

func rejAtom(ref, matchKey string) source.Atom {
	return source.Atom{Type: "rejected_alternative", Closed: true, Ref: ref, MatchKey: matchKey}
}

// syntheticGateCorpus models the real failure shape. Category terms (pipeline,
// schema, ...) recur across ~8 atoms each (df high -> not distinctive); each option
// carries a unique identity token (identN, df 1 -> distinctive). Two special
// options: a multi-identity option and a verbose option whose identity is several
// unique fillers (for the coincidental-sliver precision case). distinctiveDF here is
// 2, so the category terms are correctly excluded from the identity.
func syntheticGateCorpus() []source.Atom {
	cats := []string{"pipeline", "schema", "session", "worker", "queue", "adapter", "registry", "ledger", "boundary", "beacon"}
	atoms := make([]source.Atom, 0, 42)
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("ident%d %s %s", i, cats[i%len(cats)], cats[(i+3)%len(cats)])
		atoms = append(atoms, rejAtom(fmt.Sprintf("s%d", i), key))
	}
	atoms = append(atoms, rejAtom("multi", "sparrowdb thunderbolt session queue"))
	atoms = append(atoms, rejAtom("verbose", "moonbeam alpha beta gamma delta epsilon"))
	return atoms
}

// TestBuildGate_Precision: queries that share only recurring category vocabulary, or
// a single coincidental rare word, with atoms they do not govern must be
// not_ruled_out. These are the two shapes of the real false ruled_out.
func TestBuildGate_Precision(t *testing.T) {
	corpus := syntheticGateCorpus()

	cases := []struct{ name, topic string }{
		// Category-spread: shares several recurring terms, one or two per atom, covers
		// no atom's identity. (The 53-atom-dump shape.)
		{"category-vocabulary-spread", "rework the pipeline schema queue worker adapter boundary"},
		// Coincidental sliver: shares one rare word (gamma) that is a fraction of a
		// verbose option's identity. (The "vocabulary(df=1)" shape.)
		{"coincidental-rare-sliver", "apply gamma correction to improve the display output"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Build(corpus, c.topic)
			if v.Verdict == RuledOut {
				t.Errorf("topic %q FALSE-fired ruled_out (%s); a vocabulary coincidence is not a governing ruling", c.topic, v.Reason)
			}
		})
	}
}

// TestBuildGate_Recall: naming an option's distinctive identity must fire ruled_out
// on it. Solo (a focused option that IS its identity term) and multi-term both.
func TestBuildGate_Recall(t *testing.T) {
	corpus := syntheticGateCorpus()

	cases := []struct{ name, topic, wantRef string }{
		{"solo-identity", "let us reconsider ident7 for the redesign", "s7"},
		{"multi-identity", "switch back to sparrowdb thunderbolt", "multi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Build(corpus, c.topic)
			if v.Verdict != RuledOut {
				t.Fatalf("topic %q must fire ruled_out (naming %s's identity), got %q (%s)", c.topic, c.wantRef, v.Verdict, v.Reason)
			}
			cited := false
			for _, g := range v.GoverningDecisions {
				if g.Ref == c.wantRef && g.DerivedForce == Binding {
					cited = true
				}
			}
			if !cited {
				t.Errorf("topic %q must cite %s as binding, got %+v", c.topic, c.wantRef, v.GoverningDecisions)
			}
		})
	}
}

// --- Empirical prod-scale validation against the REAL binding feed ---
//
// Env-gated: set LEMA_REAL_CORPUS to a closed.hosted.json (the F8 feed). Skips in CI
// and in the public lema-mcp mirror (no real data is checked in). This is the
// authoritative precision/recall gate — run it before shipping a gate change:
//
//	LEMA_REAL_CORPUS=~/Projects/lema/lema/.lema/closed.hosted.json \
//	  go test ./internal/verdict/ -run RealCorpus -v

func loadRealCorpus(t *testing.T) []source.Atom {
	t.Helper()
	path := os.Getenv("LEMA_REAL_CORPUS")
	if path == "" {
		t.Skip("set LEMA_REAL_CORPUS to the real closed.hosted.json to run the prod-scale gate")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Atoms []struct{ Ref, Type, MatchKey string } `json:"atoms"`
	}
	var rawDoc struct {
		Atoms []map[string]any `json:"atoms"`
	}
	if err := json.Unmarshal(raw, &rawDoc); err != nil {
		t.Fatal(err)
	}
	_ = doc
	atoms := make([]source.Atom, 0, len(rawDoc.Atoms))
	for _, m := range rawDoc.Atoms {
		mk, _ := m["match_key"].(string)
		ref, _ := m["ref"].(string)
		typ, _ := m["type"].(string)
		atoms = append(atoms, source.Atom{Ref: ref, Type: typ, MatchKey: mk, Closed: true})
	}
	if len(atoms) < 300 {
		t.Fatalf("real corpus too small (%d) to be prod-representative", len(atoms))
	}
	return atoms
}

// TestBuildGate_RealCorpus_Precision: the live specimen that motivated the fix (it
// fired ruled_out over 51 atoms citing an unrelated decision) plus other pain-log
// topics must be not_ruled_out at real scale.
func TestBuildGate_RealCorpus_Precision(t *testing.T) {
	atoms := loadRealCorpus(t)
	negatives := []struct{ name, topic string }{
		{"live-middle-verdict-specimen", "add a middle verdict to check_decided: when a closed atom only shares vocabulary with the query but does not govern it, surface related context instead of ruled_out; reserve ruled_out for structural matches"},
		{"propose-thin-router", "add a propose verb as a thin router over the record_decision handler"},
		{"gate-runs-on-flag", "gate the runs timeline view on the run-state feature flag"},
	}
	for _, c := range negatives {
		t.Run(c.name, func(t *testing.T) {
			if v := Build(atoms, c.topic); v.Verdict == RuledOut {
				t.Errorf("topic %q FALSE-fired ruled_out at real scale (%s), governing=%d", c.topic, v.Reason, len(v.GoverningDecisions))
			}
		})
	}
}

// TestBuildGate_RealCorpus_SelfRecall: verbatim relitigation (an option's own match
// key) must fire, except atoms whose key is entirely common vocabulary (no
// distinctive identity), which must stay a small minority.
func TestBuildGate_RealCorpus_SelfRecall(t *testing.T) {
	atoms := loadRealCorpus(t)
	df, n := OptionDF(atoms)
	dd := distinctiveDF(n)
	hasIdentity := func(a source.Atom) bool {
		for _, term := range gateTerms(matchKeyFor(a)) {
			if df[term] <= dd {
				return true
			}
		}
		return false
	}
	fired, noIdentity := 0, 0
	for _, a := range atoms {
		if !hasIdentity(a) {
			noIdentity++
			continue
		}
		if Build(atoms, a.MatchKey).Verdict == RuledOut {
			fired++
		}
	}
	eligible := len(atoms) - noIdentity
	rate := float64(fired) / float64(eligible)
	t.Logf("self-recall: %d/%d eligible fire verbatim (%.1f%%); %d atoms have no distinctive identity", fired, eligible, 100*rate, noIdentity)
	if rate < 0.99 {
		t.Errorf("self-recall %.1f%% < 99%% — the gate is dropping genuine relitigations", 100*rate)
	}
	if noIdentity > len(atoms)/10 {
		t.Errorf("%d/%d atoms have no distinctive identity — distinctiveDF may be too tight", noIdentity, len(atoms))
	}
}

// TestBuildGate_RealCorpus_BoundedFanout guards the regression that matters: the
// pre-gate specimen governed 51 atoms (a 63KB dump). Real topics must govern only a
// handful. KNOWN LIMITATION (ea22dd9a): a solo COMMON-WORD identity can still fire
// gov=1 (e.g. "architecture"/"relay"/"audit") — the pure-lexical limit, deferred to
// semantic check_approach. What must never regress is the dozens-of-atoms mass-fire.
func TestBuildGate_RealCorpus_BoundedFanout(t *testing.T) {
	atoms := loadRealCorpus(t)
	topics := []string{
		"add a middle verdict to check_decided: surface related context instead of ruled_out for a vocabulary-only match",
		"add a propose verb as a thin router over the record_decision handler",
		"speed up the SVG render in the architecture overview page",
		"our webhook keeps dropping deliveries, add a retry with backoff",
	}
	for _, tp := range topics {
		if g := Governing(atoms, tp); len(g) > 4 {
			t.Errorf("topic %q governs %d atoms — mass-firing regression (pre-gate specimen governed 51)", tp, len(g))
		}
	}
}
