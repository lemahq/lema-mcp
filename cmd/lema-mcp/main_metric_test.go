package main

import (
	"context"
	"testing"

	"github.com/lemahq/lema-mcp/internal/adr"
	"github.com/lemahq/lema-mcp/internal/source"
)

func TestSearchDecisionsLocalUsage(t *testing.T) {
	src = source.NewLocal([]adr.ADR{
		{Number: 8, Title: "Data layer", Status: "accepted",
			Body: "## Context\nThe audit log needs ACID writes.\n\n## Decision\nWe chose PostgreSQL.\n\n## Alternatives considered\nMongoDB rejected: eventual consistency cannot guarantee the audit trail."},
	})
	repoName = "test/repo"

	_, out, err := searchDecisions(context.Background(), nil, searchInput{Query: "why postgres", K: 8})
	if err != nil {
		t.Fatal(err)
	}
	if out.TokensUsed <= 0 || out.Usage.AtomsTokens != out.TokensUsed {
		t.Fatalf("atoms_tokens %d should equal tokens_used %d (>0)", out.Usage.AtomsTokens, out.TokensUsed)
	}
	if out.Usage.SourceTokens < out.Usage.AtomsTokens {
		t.Errorf("source_tokens (%d) should be >= atoms_tokens (%d)", out.Usage.SourceTokens, out.Usage.AtomsTokens)
	}
	if out.Usage.TokensSaved != out.Usage.SourceTokens-out.Usage.AtomsTokens {
		t.Errorf("tokens_saved mismatch")
	}
	if out.Usage.CompressionRatio < 1 {
		t.Errorf("compression_ratio %v should be >= 1 on a large ADR", out.Usage.CompressionRatio)
	}
}
