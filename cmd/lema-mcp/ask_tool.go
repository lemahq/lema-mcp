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
	N         int    `json:"n"`
	Ref       string `json:"ref"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	Locator   string `json:"locator,omitempty"`
	URL       string `json:"url,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

type askOutput struct {
	Scope   string         `json:"scope"`
	Answer  string         `json:"answer"`
	Sources []askSourceOut `json:"sources"`
	Usage   source.AskUsage `json:"usage"`
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
		sources[i] = askSourceOut{
			N: s.N, Ref: s.Ref, Type: s.Type, Text: s.Text,
			Locator: s.Locator, URL: s.URL, Workspace: s.Workspace,
		}
	}
	out := askOutput{Scope: res.Scope, Answer: res.Answer, Sources: sources, Usage: res.Usage}
	logUsage("ask", in.Query, len(sources), out)
	return nil, out, nil
}
