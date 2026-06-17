package verdict

import (
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
		// no-regression fallback: a closed atom with an unreadable type is still
		// in the no-go set, so it must bind, not silently drop to advisory.
		{source.Atom{Closed: true, Type: ""}, Binding},
		{source.Atom{Closed: false, Type: ""}, Advisory},
	}
	for _, c := range cases {
		if got := DeriveForce(c.a); got != c.want {
			t.Errorf("DeriveForce(type=%q closed=%v) = %q, want %q", c.a.Type, c.a.Closed, got, c.want)
		}
	}
}
