package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lemahq/lema-mcp/internal/docs"
)

// fitDocsBudget keeps the highest-ranked doc hits whose cumulative token
// estimate fits the budget — the docs-side twin of fitBudget, same flag.
func fitDocsBudget(hits []docs.Hit, budget int) ([]docs.Hit, int, bool) {
	used := 0
	kept := make([]docs.Hit, 0, len(hits))
	for _, h := range hits {
		t := (len(h.Text) + 3) / 4
		if used+t > budget && len(kept) > 0 {
			return kept, used, true
		}
		kept = append(kept, h)
		used += t
	}
	return kept, used, false
}

// clipTokens truncates s to a ~budget-token prefix (4 chars/token estimate,
// rune-safe), reporting whether it clipped.
func clipTokens(s string, budget int) (string, bool) {
	if budget <= 0 {
		budget = 1500
	}
	limit := budget * 4
	if len(s) <= limit {
		return s, false
	}
	r := []rune(s)
	if len(r) > limit {
		r = r[:limit]
	}
	return string(r) + "\n…", true
}

// ── MCP tools (ADR-0055) ────────────────────────────────────────────────────
// search_docs / get_doc are the agent-side token-savings surface: sectioned,
// budgeted retrieval of project markdown instead of raw full-file Reads.

type docsSearchInput struct {
	Query     string `json:"query" jsonschema:"what you want to know from this repo's project docs"`
	K         int    `json:"k,omitempty" jsonschema:"max sections to consider (default 8)"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"token budget for the returned sections (default 1500)"`
}

type docsSearchOutput struct {
	Repo       string     `json:"repo"`
	Docs       []docs.Hit `json:"docs"`
	TokensUsed int        `json:"tokens_used"`
	Truncated  bool       `json:"truncated"`
}

func searchDocs(_ context.Context, _ *mcp.CallToolRequest, in docsSearchInput) (*mcp.CallToolResult, docsSearchOutput, error) {
	if docsStore == nil {
		return nil, docsSearchOutput{}, errors.New("no project docs indexed in this mode")
	}
	k := in.K
	if k <= 0 {
		k = 8
	}
	budget := in.MaxTokens
	if budget <= 0 {
		budget = 1500
	}
	kept, used, truncated := fitDocsBudget(docsStore.Search(in.Query, k), budget)
	out := docsSearchOutput{Repo: repoName, Docs: kept, TokensUsed: used, Truncated: truncated}
	logUsage("search_docs", in.Query, len(kept), out)
	return nil, out, nil
}

type getDocInput struct {
	Path      string `json:"path" jsonschema:"repo-relative doc path, e.g. from a search_docs hit"`
	Section   string `json:"section,omitempty" jsonschema:"a heading — return only that section and its children"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"token budget for the returned content (default 1500)"`
}

type getDocOutput struct {
	Doc       docs.Doc `json:"doc"`
	Body      string   `json:"body"`
	Truncated bool     `json:"truncated"`
}

func getDoc(_ context.Context, _ *mcp.CallToolRequest, in getDocInput) (*mcp.CallToolResult, getDocOutput, error) {
	if docsStore == nil {
		return nil, getDocOutput{}, errors.New("no project docs indexed in this mode")
	}
	d, body, ok := docsStore.Get(in.Path)
	if !ok {
		return nil, getDocOutput{}, fmt.Errorf("doc %q not found — use search_docs or the docs listing for valid paths", in.Path)
	}
	if in.Section != "" {
		s, ok := docsStore.Section(in.Path, in.Section)
		if !ok {
			return nil, getDocOutput{}, fmt.Errorf("section %q not found in %s (headings: %v)", in.Section, in.Path, d.Headings)
		}
		body = s
	}
	body, truncated := clipTokens(body, in.MaxTokens)
	out := getDocOutput{Doc: d, Body: body, Truncated: truncated}
	logUsage("get_doc", in.Path, 1, out)
	return nil, out, nil
}
