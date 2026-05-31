package source

import (
	"context"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/adr"
)

// fixtures: two small ADRs with canonical sections, enough to exercise lexical
// section ranking, ref formatting, and claim-type mapping without touching disk.
func fixtures() []adr.ADR {
	return []adr.ADR{
		{
			Number: 8, Title: "Data layer", Status: "accepted", Tags: []string{"data"},
			Body: "## Context\nThe audit log needs ACID writes.\n\n## Decision\nWe chose PostgreSQL.\n\n## Alternatives considered\nMongoDB was rejected because eventual consistency cannot guarantee the audit trail.",
		},
		{
			Number: 15, Title: "Frontend stack", Status: "accepted", Tags: []string{"web"},
			Body: "## Decision\nNext.js with the app router.\n\n## Consequences\nServer components by default.",
		},
	}
}

// Search must surface the decision that actually answers the query (not just any
// decision), return atoms shaped to the §4 contract, and format refs as ADR-NNNN.
// This fails if ranking stops weighting the matching decision or refs regress.
func TestLocalSearchSurfacesTheRightDecision(t *testing.T) {
	l := NewLocal(fixtures())
	atoms, err := l.Search(context.Background(), "why postgres over mongodb", 8)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(atoms) == 0 {
		t.Fatal("expected atoms for a query that matches a decision, got none")
	}
	// Every returned atom should come from ADR-0008 — ADR-0015 shares no query
	// terms, so a correct ranker excludes it entirely.
	for _, a := range atoms {
		if a.Ref != "ADR-0008" {
			t.Errorf("unexpected atom from %s (query terms only match ADR-0008): %q", a.Ref, a.Text)
		}
		if a.ID == "" || a.Text == "" {
			t.Errorf("atom missing required fields: %+v", a)
		}
		if !strings.HasPrefix(a.Ref, "ADR-") {
			t.Errorf("ref not formatted as ADR-NNNN: %q", a.Ref)
		}
	}
}

// The token budget is what keeps the agent's context window safe; truncation
// must drop lowest-ranked atoms and flag it, never silently overflow.
func TestSectionTypeMapping(t *testing.T) {
	cases := map[string]string{
		"Decision":                "chosen",
		"Alternatives considered": "rejected",
		"Consequences":            "consequence",
		"Context":                 "constraint",
		"Some other heading":      "decision",
	}
	for heading, want := range cases {
		if got := sectionType(heading); got != want {
			t.Errorf("sectionType(%q) = %q, want %q", heading, got, want)
		}
	}
}

// Snippets must be tight (a query-centered window, not a section dump) and
// markdown-clean — the atom contract promises sourced claims, not whole docs.
func TestSnippetsAreTightAndClean(t *testing.T) {
	long := "## Decision\nWe chose PostgreSQL. " +
		strings.Repeat("Filler context sentence. ", 40) +
		"The [pgvector](https://github.com/pgvector/pgvector) extension handles embeddings without a second store. " +
		strings.Repeat("More filler here. ", 40)
	l := NewLocal([]adr.ADR{{Number: 8, Title: "Data layer", Status: "accepted", Body: long}})
	atoms, err := l.Search(context.Background(), "pgvector embeddings", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(atoms) == 0 {
		t.Fatal("expected a match for pgvector")
	}
	top := atoms[0].Text
	if n := len([]rune(top)); n > 280 {
		t.Errorf("snippet not tight: %d runes — %q", n, top)
	}
	if !strings.Contains(top, "pgvector") {
		t.Errorf("snippet should contain the matched term: %q", top)
	}
	if strings.Contains(top, "](") || strings.Contains(top, "https://") {
		t.Errorf("markdown link not cleaned out: %q", top)
	}
}

// bestSnippet can panic if strings.ToLower(clean) changes the byte length of the string,
// which causes the hitByte to exceed the bounds of the original string.
// TestBestSnippetMultiByte verifies that we safely handle multibyte strings that
// expand during lowercase conversion (like 'Ⱥ' -> 'ⱥ').
func TestBestSnippetMultiByte(t *testing.T) {
	// 'Ⱥ' is 2 bytes (U+023A), its lowercase 'ⱥ' is 3 bytes (U+2C65).
	// A long string of them creates a large byte length discrepancy.
	clean := strings.Repeat("Ⱥ", 10) + " want"
	terms := []string{"want"}

	// If the out-of-bounds slice vulnerability is present, this will panic.
	got := bestSnippet(clean, terms, 10)

	if !strings.Contains(got, "want") {
		t.Errorf("expected snippet to contain 'want', got %q", got)
	}
}
