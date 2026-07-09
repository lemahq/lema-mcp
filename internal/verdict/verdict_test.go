package verdict

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

func atom(matchKey string, closed bool, typ string) source.Atom {
	return source.Atom{MatchKey: matchKey, Ref: matchKey, Closed: closed, Type: typ}
}

func TestBuild_RuledOutOnBindingMatch(t *testing.T) {
	closed := []source.Atom{atom("redis cache", true, "rejected_alternative")}
	v := Build(closed, "should we switch to a redis cache layer")
	if v.Verdict != RuledOut {
		t.Fatalf("want ruled_out, got %q (reason: %s)", v.Verdict, v.Reason)
	}
	if len(v.GoverningDecisions) != 1 || v.GoverningDecisions[0].DerivedForce != Binding {
		t.Fatalf("want one binding governing decision, got %+v", v.GoverningDecisions)
	}
	if v.Reason == "" {
		t.Fatal("ruled_out must carry a reason")
	}
}

func TestBuild_HistoricalMatchIsContextNotStop(t *testing.T) {
	// A superseded (historical) match is context, not a hard stop. It must still
	// be surfaced, but the verdict is not ruled_out.
	hist := atom("redis cache", true, "superseded")
	v := Build([]source.Atom{hist}, "add a redis cache here")
	if v.Verdict == RuledOut {
		t.Fatalf("a historical-only match must NOT be ruled_out; got %+v", v)
	}
	if len(v.GoverningDecisions) != 1 || v.GoverningDecisions[0].DerivedForce != Historical {
		t.Fatalf("want one historical governing decision surfaced, got %+v", v.GoverningDecisions)
	}
}

func TestBuild_RuledOutCitesBindingNotTopRankedHistorical(t *testing.T) {
	// Dogfood regression (2026-07-07, d_31fe20): a superseded lineage atom can
	// outscore the binding rejection lexically. The verdict is still ruled_out (a
	// binding match exists), but the cited "ruled out by X" ref MUST be a binding
	// decision — citing the historical top match told the agent a superseded
	// decision governs.
	hist := source.Atom{MatchKey: "living documentation graph layer", Ref: "d_hist", Closed: true, Type: "chosen"}
	bind := source.Atom{MatchKey: "documentation graph", Ref: "d_bind", Closed: true, Type: "rejected_alternative"}
	v := Build([]source.Atom{hist, bind}, "build an internal living documentation graph layer")
	if v.Verdict != RuledOut {
		t.Fatalf("binding match present: want ruled_out, got %q (reason: %s)", v.Verdict, v.Reason)
	}
	if len(v.GoverningDecisions) != 2 {
		t.Fatalf("both matches must be surfaced, got %+v", v.GoverningDecisions)
	}
	if v.GoverningDecisions[0].Ref != "d_hist" || v.GoverningDecisions[0].DerivedForce != Historical {
		t.Fatalf("precondition broken: historical atom must outrank the binding one, got %+v", v.GoverningDecisions)
	}
	if !strings.Contains(v.Reason, "d_bind") || strings.Contains(v.Reason, "d_hist") {
		t.Fatalf("ruled_out must cite the binding ref, never a historical one; got reason %q", v.Reason)
	}
}

func TestBuild_NotRuledOutWhenNoMatch(t *testing.T) {
	v := Build([]source.Atom{atom("redis cache", true, "rejected_alternative")}, "pick a logging library")
	if v.Verdict != NotRuledOut || len(v.GoverningDecisions) != 0 {
		t.Fatalf("want clean not_ruled_out, got %+v", v)
	}
}

func TestIncompleteAndErrored(t *testing.T) {
	if NewIncomplete("hosted not synced").Verdict != Incomplete {
		t.Fatal("NewIncomplete must set verdict incomplete")
	}
	if NewErrored("fetch failed").Verdict != Errored {
		t.Fatal("NewErrored must set verdict error")
	}
}

func TestDeriveForce(t *testing.T) {
	cases := []struct {
		a    source.Atom
		want Force
	}{
		{source.Atom{Closed: true, Type: "rejected_alternative"}, Binding},
		{source.Atom{Closed: true, Type: "rejected"}, Binding},
		{source.Atom{Closed: true, Type: "superseded"}, Historical},
		{source.Atom{Closed: true, Type: "deprecated"}, Historical},
		{source.Atom{Closed: true, Type: "advisory"}, Advisory},
		// fail-advisory fallback (ADR-0124/0125): a closed atom whose type we
		// cannot read must NOT manufacture a binding hard stop. The write-path
		// risk model inverted the old "default to binding is non-muting" rule —
		// a forged or garbled closed atom that auto-binds is the gun, so a
		// binding force is reserved for an explicitly-typed rejected alternative;
		// everything unrecognized degrades to advisory context, not a ruling.
		{source.Atom{Closed: true, Type: ""}, Advisory},
		{source.Atom{Closed: true, Type: "garbled-unknown"}, Advisory},
		{source.Atom{Closed: false, Type: ""}, Advisory},
		// The real-world atom that hit the old default→binding fallback: the
		// capture store marks a SUPERSEDED decision's chosen direction Closed but
		// keeps Type="chosen" (source/capture.go). It is a superseded lineage =
		// HISTORICAL context, NOT a hard ruling — the rejected ALTERNATIVES are the
		// no-go set — so it resolves historical (not_ruled_out), not binding. The
		// plan-guard's never-reopen enforcement keys on Closed directly (not
		// DeriveForce), so it still flags reopening a superseded choice.
		{source.Atom{Closed: true, Type: "chosen"}, Historical},
		// A non-closed chosen direction (the live decision's own choice) is not a
		// constraint at all → advisory.
		{source.Atom{Closed: false, Type: "chosen"}, Advisory},
	}
	for _, c := range cases {
		if got := DeriveForce(c.a); got != c.want {
			t.Errorf("DeriveForce(type=%q closed=%v) = %q, want %q", c.a.Type, c.a.Closed, got, c.want)
		}
	}
}

// TestFindingKindAndLabelDisjoint ensures FindingKind and Label vocabularies
// never collide — ruling out via FindingKind must never appear in the contract
// stream (tripwire for ruled_out bleeding into lema verify findings).
func TestFindingKindAndLabelDisjoint(t *testing.T) {
	findingKinds := []FindingKind{ClaimFound, ClaimNotFound, UndescribedChange}
	labels := []Label{RuledOut, NotRuledOut, Incomplete, Errored}

	findingSet := make(map[string]bool)
	for _, fk := range findingKinds {
		findingSet[string(fk)] = true
	}

	for _, lbl := range labels {
		if findingSet[string(lbl)] {
			t.Errorf("collision: Label %q appears in FindingKind vocabulary", lbl)
		}
	}
}

// TestFindingKindWireFormat verifies JSON round-trip of each FindingKind
// constant's string value, pinning the contract wire format.
func TestFindingKindWireFormat(t *testing.T) {
	cases := []struct {
		name    string
		kind    FindingKind
		wantStr string
	}{
		{"ClaimFound", ClaimFound, "claim_found"},
		{"ClaimNotFound", ClaimNotFound, "claim_not_found"},
		{"UndescribedChange", UndescribedChange, "undescribed_change"},
	}

	for _, c := range cases {
		// Marshal the typed value to JSON
		data, err := json.Marshal(c.kind)
		if err != nil {
			t.Errorf("%s: json.Marshal failed: %v", c.name, err)
			continue
		}

		// Assert the emitted bytes are the literal string in quotes
		wantJSON := []byte(`"` + c.wantStr + `"`)
		if string(data) != string(wantJSON) {
			t.Errorf("%s: json.Marshal = %q, want %q", c.name, string(data), string(wantJSON))
		}

		// Unmarshal back into the type and assert equality with the original
		var got FindingKind
		err = json.Unmarshal(data, &got)
		if err != nil {
			t.Errorf("%s: json.Unmarshal failed: %v", c.name, err)
			continue
		}

		if got != c.kind {
			t.Errorf("%s: round-trip got %q, want %q", c.name, got, c.kind)
		}
	}
}

// TestCardStateWireFormat verifies JSON round-trip of each CardState
// constant's string value, pinning the contract wire format.
func TestCardStateWireFormat(t *testing.T) {
	cases := []struct {
		name    string
		state   CardState
		wantStr string
	}{
		{"CardChecked", CardChecked, "checked"},
		{"CardTooGeneral", CardTooGeneral, "too_general"},
		{"CardIncomplete", CardIncomplete, "incomplete"},
		{"CardErrored", CardErrored, "error"},
	}

	for _, c := range cases {
		// Marshal the typed value to JSON
		data, err := json.Marshal(c.state)
		if err != nil {
			t.Errorf("%s: json.Marshal failed: %v", c.name, err)
			continue
		}

		// Assert the emitted bytes are the literal string in quotes
		wantJSON := []byte(`"` + c.wantStr + `"`)
		if string(data) != string(wantJSON) {
			t.Errorf("%s: json.Marshal = %q, want %q", c.name, string(data), string(wantJSON))
		}

		// Unmarshal back into the type and assert equality with the original
		var got CardState
		err = json.Unmarshal(data, &got)
		if err != nil {
			t.Errorf("%s: json.Unmarshal failed: %v", c.name, err)
			continue
		}

		if got != c.state {
			t.Errorf("%s: round-trip got %q, want %q", c.name, got, c.state)
		}
	}
}
