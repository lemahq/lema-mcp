package main

import (
	"fmt"
	"strings"

	"github.com/lemahq/lema-mcp/internal/source"
)

// sourceReceipt renders ONE honest line of trust signal for a cited source,
// derived purely (no model) from fields the api already serves. It never
// invents state: status is the ADR-0083 decision rollup the wire carries;
// rejected_alternatives are summarized (not verbatim) so they read as "ruled
// out", not a quote; relevance is cosine similarity, never "confidence". Empty
// when there is nothing honest to add.
func sourceReceipt(s source.AskSource) string {
	var parts []string
	switch strings.ToLower(s.Status) {
	case "superseded", "deprecated", "rejected":
		parts = append(parts, s.Status+" — do not cite as current")
	}
	if len(s.RejectedAlternatives) > 0 {
		parts = append(parts, "ruled out: "+strings.Join(s.RejectedAlternatives, "; ")+" (do not re-propose)")
	}
	if s.Relevance != nil {
		parts = append(parts, fmt.Sprintf("relevance %.2f (cosine)", *s.Relevance))
	}
	return strings.Join(parts, " · ")
}

// roiNote renders the token-ROI meter as one honest sentence (the "reduced
// usage" proof) ONLY for a grounded answer. On abstain the api zeroes the meter,
// so abstained==true (or a zero source baseline) returns "" — a "saved 0 tokens"
// line next to "no answer" would be noise.
func roiNote(u source.AskUsage, abstained bool) string {
	if abstained || u.SourceTokens == 0 {
		return ""
	}
	return fmt.Sprintf("served %d atom-tokens vs %d source-body tokens (~%.0fx tighter)",
		u.AtomsTokens, u.SourceTokens, u.CompressionRatio)
}
