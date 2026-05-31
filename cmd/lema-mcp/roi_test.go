package main

import (
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

func TestEstimateTokens(t *testing.T) {
	for _, c := range []struct {
		in   string
		want int
	}{{"", 0}, {"abcd", 1}, {"12345678", 2}} {
		if got := estimateTokens(c.in); got != c.want {
			t.Errorf("estimateTokens(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestLocalROI(t *testing.T) {
	atoms := []source.Atom{
		{Text: "we chose postgres", Ref: "ADR-0008"},
		{Text: "mongo rejected", Ref: "ADR-0008"}, // same doc -> counted once
	}
	bodyTokens := map[string]int{"ADR-0008": 100}
	u := localROI(atoms, bodyTokens)

	wantAtoms := estimateTokens("we chose postgres") + estimateTokens("mongo rejected")
	if u.AtomsTokens != wantAtoms {
		t.Errorf("AtomsTokens = %d, want %d", u.AtomsTokens, wantAtoms)
	}
	if u.SourceDecisions != 1 {
		t.Errorf("SourceDecisions = %d, want 1 (deduped)", u.SourceDecisions)
	}
	if u.SourceTokens != 100 || u.TokensSaved != 100-wantAtoms {
		t.Errorf("SourceTokens=%d TokensSaved=%d, want 100 / %d", u.SourceTokens, u.TokensSaved, 100-wantAtoms)
	}
	if u.CompressionRatio < 1 {
		t.Errorf("CompressionRatio = %v, want >= 1", u.CompressionRatio)
	}
}
