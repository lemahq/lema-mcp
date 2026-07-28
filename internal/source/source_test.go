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

// bestSnippet must not panic when strings.ToLower(clean) changes the byte length
// of the string, which pushes the byte index hitByte past the end of clean. This
// guards the snippet path against a DoS panic on input that expands during
// lowercasing ('Ⱥ' U+023A, 2 bytes → 'ⱥ' U+2C65, 3 bytes). Credit: Jules (DoS report).
func TestBestSnippetMultiByteNoPanic(t *testing.T) {
	clean := strings.Repeat("Ⱥ", 10) + " want"
	got := bestSnippet(clean, []string{"want"}, 10) // panics if hitByte is sliced into clean
	if !strings.Contains(got, "want") {
		t.Errorf("expected snippet to contain the matched term, got %q", got)
	}
}

// splitSections accumulates each section's body with a strings.Builder (ported
// from lema-mcp#50, R4). This pins the accumulated text exactly — blank lines
// inside a section, trailing newlines, and a ##/### heading transition all in
// one body — so a Builder/Reset off-by-one in newline handling would fail here
// even though it wouldn't visibly break search ranking.
func TestSplitSectionsAccumulatesTextAcrossBlankLinesAndHeadingTransitions(t *testing.T) {
	body := "## Context\n" +
		"First line of context.\n" +
		"\n" +
		"Second line after a blank line.\n" +
		"\n\n" +
		"### Nested detail\n" +
		"Detail line one.\n" +
		"Detail line two.\n" +
		"\n"
	secs := splitSections(body)
	if len(secs) != 2 {
		t.Fatalf("expected 2 sections, got %d: %+v", len(secs), secs)
	}
	ctx := secs[0]
	if ctx.heading != "Context" || ctx.level != 2 {
		t.Fatalf("section 0: got heading=%q level=%d, want Context/2", ctx.heading, ctx.level)
	}
	wantCtx := "First line of context.\n\nSecond line after a blank line."
	if ctx.text != wantCtx {
		t.Errorf("section 0 text = %q, want %q", ctx.text, wantCtx)
	}
	detail := secs[1]
	if detail.heading != "Nested detail" || detail.level != 3 {
		t.Fatalf("section 1: got heading=%q level=%d, want Nested detail/3", detail.heading, detail.level)
	}
	wantDetail := "Detail line one.\nDetail line two."
	if detail.text != wantDetail {
		t.Errorf("section 1 text = %q, want %q", detail.text, wantDetail)
	}
}

// A client asking for more than the cap must get the cap, not the default —
// the workbench sidebar lists every decision via limit=300, and resetting an
// over-cap ask to 50 silently hides the newest ADRs from the Docs tab.
func TestListOverCapLimitClampsToCapNotDefault(t *testing.T) {
	many := make([]adr.ADR, 60)
	for i := range many {
		many[i] = adr.ADR{Number: i + 1, Title: "T", Status: "accepted", Body: "## Decision\nx."}
	}
	l := NewLocal(many)
	out, err := l.List(context.Background(), "", 300)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 60 {
		t.Fatalf("limit=300 over 60 decisions: got %d, want all 60 (over-cap ask must clamp to the cap, not reset to the default 50)", len(out))
	}
}
