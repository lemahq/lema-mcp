package main

import (
	"context"
	"regexp"
	"strconv"

	"github.com/lemahq/lema-mcp/internal/source"
)

// localUsage mirrors retrieval.Usage (ADR-0040) so the free local server reports
// the same tokens-saved shape as the hosted /retrieve path — computed here without
// the pgx dependency internal/retrieval carries, keeping the lean binary lean.
type localUsage struct {
	AtomsTokens      int     `json:"atoms_tokens"`
	SourceDecisions  int     `json:"source_decisions"`
	SourceTokens     int     `json:"source_tokens"`
	TokensSaved      int     `json:"tokens_saved"`
	CompressionRatio float64 `json:"compression_ratio"`
}

// estimateTokens is lema's shared (len+3)/4 heuristic (matches retrieval.EstimateTokens).
func estimateTokens(s string) int { return (len(s) + 3) / 4 }

var roiRefRe = regexp.MustCompile(`^ADR-0*(\d+)$`)

// adrNumFromRef parses "ADR-0008" -> 8. Non-ADR refs contribute no baseline.
func adrNumFromRef(ref string) (int, bool) {
	m := roiRefRe.FindStringSubmatch(ref)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// localROI computes the tokens-saved of a local search: the returned atoms' token
// cost versus the full bodies of the distinct source ADRs they cite. bodyTokensByRef
// maps an atom Ref to its source body's token count; a ref absent from the map
// contributes 0. The baseline is the documents the answer draws from — what the agent
// would otherwise read — never the whole corpus.
func localROI(atoms []source.Atom, bodyTokensByRef map[string]int) localUsage {
	atomsTokens := 0
	srcs := map[string]struct{}{}
	for _, a := range atoms {
		atomsTokens += estimateTokens(a.Text)
		if a.Ref != "" {
			srcs[a.Ref] = struct{}{}
		}
	}
	total := 0
	for ref := range srcs {
		total += bodyTokensByRef[ref]
	}
	u := localUsage{AtomsTokens: atomsTokens, SourceDecisions: len(srcs), SourceTokens: total, TokensSaved: total - atomsTokens}
	if atomsTokens > 0 {
		u.CompressionRatio = float64(total) / float64(atomsTokens)
	}
	return u
}

// localSearchROI resolves the full bodies of the distinct ADRs the kept atoms cite
// (via the local source's Get) and returns the tokens-saved block. Best-effort: a
// ref that does not resolve (e.g. hosted/search-only mode, or a non-ADR locator)
// contributes no baseline, so Usage degrades to zero rather than erroring.
func localSearchROI(ctx context.Context, atoms []source.Atom) localUsage {
	bodyTokensByRef := map[string]int{}
	seen := map[int]bool{}
	for _, a := range atoms {
		n, ok := adrNumFromRef(a.Ref)
		if !ok || seen[n] {
			continue
		}
		seen[n] = true
		if d, err := src.Get(ctx, n); err == nil {
			bodyTokensByRef[a.Ref] = estimateTokens(d.Body)
		}
	}
	return localROI(atoms, bodyTokensByRef)
}
