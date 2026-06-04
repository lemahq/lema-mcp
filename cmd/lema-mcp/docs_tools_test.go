package main

import (
	"context"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/docs"
)

func TestFitDocsBudgetTruncates(t *testing.T) {
	// The budget is the token-savings contract (ADR-0025 via ADR-0055): the
	// tool must clip to it AND say so — silent truncation reads as "that was
	// everything" and corrupts the agent's picture of the docs.
	hits := []docs.Hit{
		{ID: "a", Text: strings.Repeat("x", 400)}, // ~100 tokens
		{ID: "b", Text: strings.Repeat("y", 400)},
		{ID: "c", Text: strings.Repeat("z", 400)},
	}
	kept, used, truncated := fitDocsBudget(hits, 150)
	if len(kept) != 1 || !truncated {
		t.Fatalf("kept=%d truncated=%v, want 1/true", len(kept), truncated)
	}
	if used == 0 {
		t.Fatal("tokens_used must report the kept size")
	}
	// And a budget that fits everything must NOT flag truncation.
	kept, _, truncated = fitDocsBudget(hits, 10000)
	if len(kept) != 3 || truncated {
		t.Fatalf("kept=%d truncated=%v, want 3/false", len(kept), truncated)
	}
}

func TestClipTokensRuneSafe(t *testing.T) {
	s, trunc := clipTokens(strings.Repeat("é", 100), 10) // multibyte: must not split a rune
	if !trunc || !strings.HasSuffix(s, "…") {
		t.Fatalf("clip = %q trunc=%v", s, trunc)
	}
}

func TestSearchDocsAndGetDocTools(t *testing.T) {
	setupDocsWorld(t) // from serve_docs_test.go — same package

	_, out, err := searchDocs(context.Background(), nil, docsSearchInput{Query: "wombat"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Docs) == 0 || out.Docs[0].Path != "docs/guide.md" {
		t.Fatalf("search_docs = %+v", out)
	}

	_, g, err := getDoc(context.Background(), nil, getDocInput{Path: "docs/guide.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.Body, "wombat docs text") || g.Doc.Title != "Guide" {
		t.Fatalf("get_doc = %+v", g)
	}

	_, gs, err := getDoc(context.Background(), nil, getDocInput{Path: "docs/guide.md", Section: "Wombat handling"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gs.Body, "wombat docs text") || strings.Contains(gs.Body, "# Guide\n\n") {
		t.Fatalf("section body = %q", gs.Body)
	}

	if _, _, err := getDoc(context.Background(), nil, getDocInput{Path: "nope.md"}); err == nil {
		t.Fatal("unknown path must error, not return empty success")
	}
}
