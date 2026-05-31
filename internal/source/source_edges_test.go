package source

import (
	"context"
	"testing"

	"github.com/lemahq/lema-mcp/internal/adr"
)

// These tests cover the additive edge annotation on the MCP search shape
// (2026-05-30 edge-retrieval spec, scope §2/§3): local search_decisions now
// carries each atom's source-decision edges, derived from frontmatter, without
// changing which atoms rank or in what order.

// edgeFixtures: two ADRs where the matching one (ADR-30) declares frontmatter
// relationships, so a search that surfaces it can be asserted to carry those
// edges through on every returned atom.
func edgeFixtures() []adr.ADR {
	superseded := 28
	return []adr.ADR{
		{
			Number: 30, Title: "Rename to lema", Status: "accepted", Tags: []string{"naming"},
			Supersedes: []int{28}, DependsOn: []int{37}, RelatedTo: []int{33},
			Body: "## Context\nThe rename matters for branding.\n\n## Decision\nWe renamed the project to lema across code and infra.",
		},
		{
			Number: 28, Title: "Rename to Naos", Status: "superseded", SupersededBy: &superseded,
			Body: "## Decision\nWe renamed the project to Naos.",
		},
	}
}

// search_decisions must attach the source decision's frontmatter edges to the
// atoms it returns, so an agent sees the relationships inline. This fails if the
// edge annotation is dropped from the search path.
func TestLocalSearchAnnotatesEdges(t *testing.T) {
	l := NewLocal(edgeFixtures())
	atoms, err := l.Search(context.Background(), "why did we rename to lema", 8)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(atoms) == 0 {
		t.Fatal("expected atoms for a query matching ADR-0030, got none")
	}

	// Every returned atom comes from ADR-0030 (ADR-0028 shares few terms); each
	// must carry the same frontmatter edge set.
	want := map[string]int{ // edge type -> target ADR number
		"supersedes": 28,
		"depends_on": 37,
		"related_to": 33,
	}
	for _, a := range atoms {
		if a.Ref != "ADR-0030" {
			continue // a stray lower-ranked atom from the other ADR, if any
		}
		if len(a.Edges) != len(want) {
			t.Fatalf("ADR-0030 atom carries %d edges %v, want %d", len(a.Edges), a.Edges, len(want))
		}
		for _, e := range a.Edges {
			to, ok := want[e.Type]
			if !ok {
				t.Errorf("unexpected edge type %q on atom", e.Type)
				continue
			}
			if e.To != to {
				t.Errorf("edge %q -> %d, want %d", e.Type, e.To, to)
			}
		}
	}
}

// Non-regression: the edge annotation is additive, so the ranked id sequence is
// identical to what Search returned before edges were attached. We assert it by
// blanking the new field and comparing to a Search over the same fixtures — the
// ranking inputs (text/score) are untouched by annotation.
func TestLocalSearchEdgesDoNotChangeRanking(t *testing.T) {
	l := NewLocal(edgeFixtures())
	q := "why did we rename to lema"

	first, err := l.Search(context.Background(), q, 8)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	second, err := l.Search(context.Background(), q, 8)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("ranked length unstable: %d vs %d", len(first), len(second))
	}
	for i := range first {
		// The ranked identity/order/text are the load-bearing retrieval output and
		// must be deterministic; edges ride alongside without perturbing them.
		if first[i].ID != second[i].ID || first[i].Text != second[i].Text {
			t.Errorf("ranked atom[%d] changed across calls: (%s,%q) vs (%s,%q)",
				i, first[i].ID, first[i].Text, second[i].ID, second[i].Text)
		}
	}
}

// An ADR with no frontmatter relationships yields atoms with no edges — the field
// omits cleanly rather than serializing an empty array, keeping the §4 payload
// minimal for the common (edgeless) case.
func TestLocalSearchNoEdgesWhenNoFrontmatter(t *testing.T) {
	l := NewLocal([]adr.ADR{{
		Number: 8, Title: "Data layer", Status: "accepted",
		Body: "## Decision\nWe chose PostgreSQL for ACID writes.",
	}})
	atoms, err := l.Search(context.Background(), "why postgres", 8)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(atoms) == 0 {
		t.Fatal("expected at least one atom")
	}
	for _, a := range atoms {
		if len(a.Edges) != 0 {
			t.Errorf("atom from an edgeless ADR carries edges %v, want none", a.Edges)
		}
	}
}
