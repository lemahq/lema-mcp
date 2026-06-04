package docs

import (
	"strings"
	"testing"
)

// The chunker is the unit get_doc's section retrieval stands on: a chunk is
// "one heading-bounded section". These tests encode the invariants that keep
// section retrieval truthful.

func TestChunkBodySplitsByHeadings(t *testing.T) {
	body := "intro before any heading\n\n# Title\n\nlead para\n\n## Section A\n\na-text\n\n### Sub A1\n\na1-text\n\n## Section B\n\nb-text\n"
	title, headings, chunks := chunkBody("docs/x.md", "doc", body)
	if title != "Title" {
		t.Fatalf("title = %q, want Title", title)
	}
	wantHeadings := []string{"Title", "Section A", "Sub A1", "Section B"}
	if strings.Join(headings, "|") != strings.Join(wantHeadings, "|") {
		t.Fatalf("headings = %v, want %v", headings, wantHeadings)
	}
	// preamble + Title-lead + A + A1 + B = 5 chunks
	if len(chunks) != 5 {
		t.Fatalf("chunks = %d, want 5: %+v", len(chunks), chunks)
	}
	if len(chunks[0].Trail) != 0 || !strings.Contains(chunks[0].Text, "intro before") {
		t.Fatalf("chunk 0 should be the trail-less preamble, got %+v", chunks[0])
	}
	// The trail carries ancestry: Sub A1 is reachable as Title > Section A > Sub A1.
	a1 := chunks[3]
	if strings.Join(a1.Trail, ">") != "Title>Section A>Sub A1" {
		t.Fatalf("a1 trail = %v", a1.Trail)
	}
}

func TestChunkBodyFenceNeverSplits(t *testing.T) {
	// A '#' inside a fenced code block is code, not a heading. A false split
	// would corrupt section retrieval — get_doc would return half a section
	// with someone's shell comment promoted to a heading.
	body := "## Real\n\nbefore\n\n```bash\n# not a heading\necho hi\n```\n\nafter\n"
	_, headings, chunks := chunkBody("docs/x.md", "doc", body)
	if len(headings) != 1 || headings[0] != "Real" {
		t.Fatalf("headings = %v, want [Real]", headings)
	}
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1 (fence must not split)", len(chunks))
	}
	if !strings.Contains(chunks[0].Text, "# not a heading") || !strings.Contains(chunks[0].Text, "after") {
		t.Fatalf("fence content and trailing prose must stay in the one chunk: %q", chunks[0].Text)
	}
}

func TestChunkBodyNoHeadings(t *testing.T) {
	// A heading-less doc (a plain README note) must still be retrievable —
	// one preamble chunk, empty trail, no title.
	title, headings, chunks := chunkBody("NOTES.md", "doc", "just prose\nmore prose\n")
	if title != "" || len(headings) != 0 {
		t.Fatalf("title=%q headings=%v, want empty", title, headings)
	}
	if len(chunks) != 1 || len(chunks[0].Trail) != 0 {
		t.Fatalf("want single trail-less chunk, got %+v", chunks)
	}
}

func TestChunkBodyDeepHeadingStaysInParent(t *testing.T) {
	// #### and deeper are content, not chunk boundaries (ADR-0055 scopes the
	// chunker to #–###): a deeply-nested doc must not shatter into fragments
	// too small to be useful retrieval units.
	body := "## S\n\ntop\n\n#### deep\n\ndeep-text\n"
	_, headings, chunks := chunkBody("docs/x.md", "doc", body)
	if len(headings) != 1 {
		t.Fatalf("headings = %v, want [S] only", headings)
	}
	if len(chunks) != 1 || !strings.Contains(chunks[0].Text, "deep-text") {
		t.Fatalf("#### must stay inside its parent chunk: %+v", chunks)
	}
}

func TestChunkBodyTrailPopsSiblings(t *testing.T) {
	// When a sibling heading opens, the previous same-or-deeper headings pop
	// off the trail — otherwise section B would claim section A as ancestor
	// and get_doc(section: "Section A") would wrongly return B's content.
	body := "## A\n\na\n\n### A1\n\na1\n\n## B\n\nb\n"
	_, _, chunks := chunkBody("docs/x.md", "doc", body)
	last := chunks[len(chunks)-1]
	if strings.Join(last.Trail, ">") != "B" {
		t.Fatalf("B trail = %v, want [B]", last.Trail)
	}
}
