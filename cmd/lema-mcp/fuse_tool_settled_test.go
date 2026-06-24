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

// settledStub returns an httptest server that answers /fuse with a `settled`
// verdict carrying n cited in-force atoms — the net-new affirmative verdict
// (ADR-0110), not a capability the retired /settled tool ever served. n drives
// the coverage of the match.
func settledStub(t *testing.T, n int) *httptest.Server {
	t.Helper()
	bc := 0.84
	sources := make([]any, n)
	for i := 0; i < n; i++ {
		sources[i] = map[string]any{
			"n": i + 1, "ref": fmt.Sprintf("ADR-%04d", 40+i), "type": "chosen",
			"text":           "we adopt concurrent transitions via startTransition",
			"binding_cosine": bc,
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "react-rfcs", "approach": "use the new transitions API",
			"verdict": "settled",
			"sources": sources,
			"how":     map[string]any{"doc_home": "https://react.dev", "topic": "we adopt concurrent transitions via startTransition"},
			"note":    "this is the recorded, in-force choice",
		})
	}))
}

// TestCheckApproachSettledVerdict encodes the ADR-0110 fold-in AS GATED BY ADR-0124:
// when /fuse returns the affirmative `settled` verdict AND coverage clears the sparse
// threshold (>= coverageAffirmThreshold matching in-force atoms), check_approach
// asserts it as a grounded fire — the cited decisions, the grounding steer, the honest
// absent-capability caveats, the affirmative verb — and does NOT attach the abstain
// upgrade CTA (settled is not an abstain). This affirmative verdict is net-new; the
// retired `settled`/`why_not_public` tools' rejection signal is what `ruled_out`
// covers (see the ruled_out tests), so the pair retired with that capability intact.
func TestCheckApproachSettledVerdict(t *testing.T) {
	ts := settledStub(t, coverageAffirmThreshold) // exactly clears the threshold
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "use the new transitions API")
	if err != nil {
		t.Fatalf("runCheckApproach: %v", err)
	}
	if out.Verdict != "settled" {
		t.Fatalf("verdict = %q, want settled — coverage clears the threshold so the affirmative verb stands", out.Verdict)
	}
	if len(out.Sources) != coverageAffirmThreshold {
		t.Fatalf("settled must cite the governing decisions, got %+v", out.Sources)
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
	// The coverage slice (slice 4) reports the affirmative as sufficient.
	if out.Coverage == nil || !out.Coverage.Sufficient {
		t.Errorf("a threshold-clearing settled must report sufficient coverage, got %+v", out.Coverage)
	}
}

// TestCheckApproachSettledSparseDemotes is the affirmative-verb gate (ADR-0124,
// design-lock): completeness is a property of the RECORD, so the affirmative
// assertion verb is gated on coverage. A `settled` match backed by a single
// (sub-threshold) atom does NOT establish "the project decided X" — a stub must not
// out-assert its coverage. The same atom is RETURNED (citation kept) but the verb is
// DOWNGRADED: the surfaced verdict drops to no_recorded_ruling-with-context (it stays
// inside the three-valued space the description promises), the affirmative grounding
// steer and caveats are withheld, and the coverage slice carries the honest
// "one matching atom on a sparse record" framing.
func TestCheckApproachSettledSparseDemotes(t *testing.T) {
	ts := settledStub(t, 1) // one matching atom — below the sparse threshold
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "use the new transitions API")
	if err != nil {
		t.Fatalf("runCheckApproach: %v", err)
	}
	// Verb downgraded: the affirmative "settled" must NOT be asserted from a stub.
	if out.Verdict == "settled" {
		t.Fatalf("a single-atom settled must DEMOTE — it must not assert the affirmative settled verb")
	}
	if out.Verdict != "no_recorded_ruling" {
		t.Fatalf("verdict = %q, want no_recorded_ruling (the demoted, in-space verb)", out.Verdict)
	}
	// Citation kept: the one matching atom is still returned as context.
	if len(out.Sources) != 1 || out.Sources[0].Ref == "" {
		t.Fatalf("the demoted settled must KEEP the matching atom as a cited source, got %+v", out.Sources)
	}
	// The affirmative steer/caveats frame an ESTABLISHED fire — they must not ride a demotion.
	if out.GroundingNote != "" {
		t.Errorf("a demoted (sparse) settled must not carry the affirmative grounding note: %q", out.GroundingNote)
	}
	if len(out.Caveats) != 0 {
		t.Errorf("a demoted (sparse) settled must not carry the established-fire caveats: %+v", out.Caveats)
	}
	// The affirmative HOW must be stripped: on settled the server points how.Topic at the
	// chosen text itself (an in-force assertion). A demoted verdict that still named the
	// sanctioned approach would re-assert exactly what the gate withholds. Only the plain
	// docs-home pointer survives.
	if out.How.Topic != "" || out.How.SanctionedAlternative != "" {
		t.Errorf("a demoted (sparse) settled must STRIP the affirmative how (Topic/SanctionedAlternative), got %+v", out.How)
	}
	if out.How.DocHome == "" {
		t.Error("the demoted output should keep the plain docs-home pointer for verify-against-the-docs")
	}
	// The server's affirmative settled note ("the recorded, in-force choice") must not pass
	// through; the honest framing lives on the coverage slice, not the top-level note.
	if out.Note != "" {
		t.Errorf("a demoted (sparse) settled must suppress the server's affirmative note, got %q", out.Note)
	}
	// The coverage slice leads with the honest framing and reports insufficiency.
	if out.Coverage == nil || out.Coverage.Sufficient {
		t.Fatalf("a sparse settled must report insufficient coverage, got %+v", out.Coverage)
	}
	if out.Coverage.Note == "" {
		t.Error("the demoted coverage slice must carry the honest 'sparse record' note")
	}
	if out.Coverage.MatchedAtoms != 1 {
		t.Errorf("coverage must report the real matched-atom count, got %d", out.Coverage.MatchedAtoms)
	}
}

// TestCheckApproachSettledZeroSourcesDemotes pins the affirmative-verb gate's
// defensive edge (review finding): a `settled` verdict that arrives with NO cited
// atoms must never fall through still labelled "settled" — the tool cannot trust a
// zero-coverage stub as the in-force choice. It demotes like any sub-threshold settled.
// (Unreachable from today's server, which always cites >=1 accepted atom; this guards
// against a future/buggy upstream re-introducing the contradiction.)
func TestCheckApproachSettledZeroSourcesDemotes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "react-rfcs", "approach": "x", "verdict": "settled",
			"sources": []any{},
			"how":     map[string]any{"doc_home": "https://react.dev", "topic": "an in-force choice"},
			"note":    "this is the recorded, in-force choice",
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
	if out.Verdict == "settled" {
		t.Fatalf("a zero-source settled must NOT remain labelled settled (no in-force claim from a stub)")
	}
	if out.Verdict != "no_recorded_ruling" {
		t.Fatalf("verdict = %q, want no_recorded_ruling", out.Verdict)
	}
	if out.How.Topic != "" {
		t.Errorf("the affirmative how must be stripped on a zero-source demotion, got %q", out.How.Topic)
	}
	if out.Coverage == nil || out.Coverage.Sufficient || out.Coverage.MatchedAtoms != 0 {
		t.Errorf("zero-source demotion must report 0 matched atoms, insufficient, got %+v", out.Coverage)
	}
}

// TestCheckApproachNoRulingStillAbstains pins that the default branch is unchanged
// by the new settled/coverage cases: a no_recorded_ruling verdict still attaches the
// honest upgrade CTA, carries no grounding note, and the coverage slice never asserts
// a reassuring negative on the empty match.
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
	if out.Coverage == nil || out.Coverage.Sufficient {
		t.Errorf("an empty-match abstain must report insufficient coverage, got %+v", out.Coverage)
	}
}
