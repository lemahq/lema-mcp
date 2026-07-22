// brief_resource.go is the server's FIRST MCP resource (F19, decision
// fa8a63f4): lema://brief, a read-only projection of the get_state_brief
// relay read for hosts that surface resources rather than tools. It is a
// thin wrapper over the same stateBrief code path (state_brief_tool.go) —
// no fetch or render logic of its own, so the tool and the resource can
// never drift. Having no arguments, it always resolves this project's
// prior run from the local F4 checkpoint; every can't-serve path (no prior
// run known, dark flag / 404, unreachable API, unresolvable workspace) is
// the tool's honest note served as the resource CONTENT, never a protocol
// error — a host attaching the resource should read state, not a failure.
// Registered only in the hosted tier (main.go), beside the verb it
// projects; further resources and templates were ruled out in fa8a63f4.
package main

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const briefResourceURI = "lema://brief"

var briefResource = &mcp.Resource{
	URI:   briefResourceURI,
	Name:  "state-brief",
	Title: "State Brief — current project",
	Description: "The scoped State Brief for this project's prior session (the relay read): " +
		"objective, last checkpoint, files in flight, settled decisions in scope (cited), " +
		"binding rejected approaches, related active runs — with every unavailable section " +
		"named in silences. Same content as the get_state_brief tool with no run argument.",
	MIMEType: "application/json",
}

func readBriefResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	out := stateBrief(ctx, "", "resource:"+briefResourceURI)
	text, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI: briefResourceURI, MIMEType: briefResource.MIMEType, Text: string(text),
		}},
	}, nil
}

func registerBriefResource(server *mcp.Server) {
	server.AddResource(briefResource, readBriefResource)
}
