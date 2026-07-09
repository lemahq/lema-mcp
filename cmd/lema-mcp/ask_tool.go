package main

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lemahq/lema-mcp/internal/source"
)

// ask_tool.go is the hosted-only `ask` MCP tool (ADR-0059 shape A): the closed
// loop's serving join. It POSTs the question to the hosted /ask and returns a
// synthesized, cited answer — the thing the local DB-less/LLM-free binary
// cannot produce. Registered only when LEMA_API_URL is set (hostedSrc != nil),
// so the wedge binary is untouched.

type askInput struct {
	Query        string   `json:"query" jsonschema:"the natural-language question about your team's decisions"`
	WorkspaceIDs []string `json:"workspace_ids,omitempty" jsonschema:"optional workspace ids to focus the search; omit to search every workspace you can see"`
}

// askSourceOut is one cited source behind a [n] in the answer. source.AskSource
// already carries the additive locator/url (ADR-0056), so an agent can follow a
// citation straight to the artifact.
type askSourceOut struct {
	N                    int      `json:"n"`
	Ref                  string   `json:"ref"`
	Type                 string   `json:"type"`
	Text                 string   `json:"text"`
	Locator              string   `json:"locator,omitempty"`
	URL                  string   `json:"url,omitempty"`
	Status               string   `json:"status,omitempty"`
	Workspace            string   `json:"workspace,omitempty"`
	RejectedAlternatives []string `json:"rejected_alternatives,omitempty"`
	Relevance            *float64 `json:"relevance,omitempty"`
	Receipt              string   `json:"receipt,omitempty"`
	// DecisionURL is the citation's stable public permalink ({web}/d/{id}) — a
	// no-signup page a human can open from a paste. Present only on
	// public-corpus answers; the authed corpus never serves a public URL.
	DecisionURL string `json:"decision_url,omitempty"`
}

// toAskSourceOut maps a wire AskSource into the tool output, attaching the
// derived one-line honesty receipt. Shared by the authed `ask` and public_ask.
func toAskSourceOut(s source.AskSource) askSourceOut {
	return askSourceOut{
		N: s.N, Ref: s.Ref, Type: s.Type, Text: s.Text,
		Locator: s.Locator, URL: s.URL, Status: s.Status, Workspace: s.Workspace,
		RejectedAlternatives: s.RejectedAlternatives, Relevance: s.Relevance,
		Receipt:     sourceReceipt(s),
		DecisionURL: s.DecisionURL,
	}
}

type askOutput struct {
	Scope   string          `json:"scope"`
	Answer  string          `json:"answer"`
	Sources []askSourceOut  `json:"sources"`
	Usage   source.AskUsage `json:"usage"`
	ROINote string          `json:"roi_note,omitempty"`
}

// askHosted handles the `ask` tool. It is only registered in hosted mode, so
// hostedSrc is non-nil here; the nil guard is belt-and-suspenders.
func askHosted(ctx context.Context, _ *mcp.CallToolRequest, in askInput) (*mcp.CallToolResult, askOutput, error) {
	if hostedSrc == nil {
		return nil, askOutput{}, fmt.Errorf("ask is hosted-only; run lema-mcp with LEMA_API_URL set")
	}
	res, err := hostedSrc.Ask(ctx, in.Query, in.WorkspaceIDs)
	if err != nil {
		return nil, askOutput{}, err
	}
	sources := make([]askSourceOut, len(res.Sources))
	for i, s := range res.Sources {
		sources[i] = toAskSourceOut(s)
	}
	out := askOutput{
		Scope: res.Scope, Answer: res.Answer, Sources: sources, Usage: res.Usage,
		ROINote: roiNote(res.Usage, len(res.Sources) == 0),
	}
	logUsage("ask", in.Query, len(sources), out)
	return nil, out, nil
}
