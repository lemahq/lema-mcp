package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestToolsMeetDirectoryCriteria verifies — over an in-memory MCP session, i.e.
// exactly what a host receives from tools/list — that every tool this binary
// ships meets the Anthropic Directory review criteria
// (https://claude.com/docs/connectors/building/review-criteria):
//   - title + readOnlyHint + destructiveHint annotations are present;
//   - the read tools are marked read-only and the one writer is not;
//   - tool names are <= 64 chars; and
//   - descriptions describe what the tool does — they do NOT instruct Claude how
//     to behave (that workflow guidance lives in the generated AGENTS.md and the
//     guard hook).
//
// WHY this matters enough to test: a dropped annotation or a re-introduced
// "call this …"-style description is a pass/fail Directory blocker and also
// degrades auto-permission UX in every host — both are silent regressions that a
// human reviewer would not reliably catch in a description diff.
func TestToolsMeetDirectoryCriteria(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "lema-mcp", Version: "test"}, nil)
	// Register every tool the binary can expose, across all modes (the conditional
	// hosted/docs tools included) — the criteria bind every tool, not just the
	// always-on ones.
	mcp.AddTool(server, searchDecisionsTool, searchDecisions)
	mcp.AddTool(server, getDecisionTool, getDecision)
	mcp.AddTool(server, listDecisionsTool, listDecisions)
	mcp.AddTool(server, getDecisionGraphTool, getDecisionGraph)
	mcp.AddTool(server, recordDecisionTool, recordDecision)
	mcp.AddTool(server, checkDecidedTool, checkDecided)
	mcp.AddTool(server, checkApproachTool, checkApproach)
	mcp.AddTool(server, askTool, askHosted)
	mcp.AddTool(server, searchDocsTool, searchDocs)
	mcp.AddTool(server, getDocTool, getDoc)
	mcp.AddTool(server, getStateBriefTool, getStateBrief)
	mcp.AddTool(server, resolveTool, resolve)
	mcp.AddTool(server, proposeTool, propose)

	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != len(directoryTools) {
		t.Fatalf("tools/list returned %d tools, want %d (a new tool must be added to directoryTools and meet the criteria)", len(res.Tools), len(directoryTools))
	}

	// record_decision and its end-state verb propose are the writers; every
	// other tool is read-only.
	writers := map[string]bool{"record_decision": true, "propose": true}
	// Phrases that instruct Claude how to behave — rejected at Directory review as
	// prompt injection. Sibling disambiguation ("use get_decision") is allowed and
	// deliberately NOT listed here.
	banned := []string{"call this", "you must", "do not re-propose", "prefer this over", "instead of reading", "before you propose", "before you write"}

	for _, tl := range res.Tools {
		if len(tl.Name) > 64 {
			t.Errorf("%s: name is %d chars, must be <= 64", tl.Name, len(tl.Name))
		}
		if tl.Annotations == nil {
			t.Errorf("%s: missing annotations (Directory requires title + readOnlyHint + destructiveHint)", tl.Name)
			continue
		}
		if tl.Annotations.Title == "" {
			t.Errorf("%s: missing annotations.title", tl.Name)
		}
		if tl.Annotations.DestructiveHint == nil {
			t.Errorf("%s: destructiveHint must be set explicitly, not left to the host's worst-case default", tl.Name)
		}
		if writers[tl.Name] {
			if tl.Annotations.ReadOnlyHint {
				t.Errorf("%s: a writer must not be readOnlyHint:true", tl.Name)
			}
			if tl.Annotations.DestructiveHint != nil && *tl.Annotations.DestructiveHint {
				t.Errorf("%s: additive writer must be destructiveHint:false (it appends/supersedes, never hard-deletes)", tl.Name)
			}
		} else if !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s: a read tool must be readOnlyHint:true so hosts can auto-approve it", tl.Name)
		}
		if tl.Description == "" {
			t.Errorf("%s: empty description", tl.Name)
		}
		low := strings.ToLower(tl.Description)
		for _, b := range banned {
			if strings.Contains(low, b) {
				t.Errorf("%s: description contains behavioral phrase %q — describe what the tool does; move workflow guidance to AGENTS.md / the guard hook", tl.Name, b)
			}
		}

		// CLASS GUARD: every property schema a host receives from tools/list must be a
		// JSON OBJECT, never a bare boolean. A bare `any` field reflects to the boolean
		// `true`, which a strict MCP client (Claude Code) rejects — failing the WHOLE
		// tools/list and hiding every tool (get_state_brief's sections/silences, the
		// 0.21.1/0.21.2 regression). This binds every current and future tool so the
		// next `any`-typed field can't silently re-break discovery.
		assertObjectPropertySchemas(t, tl.Name, "inputSchema", tl.InputSchema)
		assertObjectPropertySchemas(t, tl.Name, "outputSchema", tl.OutputSchema)
	}
}

// assertObjectPropertySchemas fails if any property schema anywhere in a tool's
// schema is a bare JSON boolean rather than an object — the form a strict MCP client
// rejects, failing the entire tools/list. A legitimate boolean `additionalProperties`
// is intentionally NOT flagged; only values inside a `properties` map are checked.
func assertObjectPropertySchemas(t *testing.T, toolName, field string, schema any) {
	t.Helper()
	if schema == nil {
		return
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("%s %s: marshal schema: %v", toolName, field, err)
	}
	var node any
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("%s %s: unmarshal schema: %v", toolName, field, err)
	}
	walkPropertySchemas(t, toolName, field, node)
}

func walkPropertySchemas(t *testing.T, toolName, field string, node any) {
	t.Helper()
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	if props, ok := m["properties"].(map[string]any); ok {
		for name, val := range props {
			if _, isObject := val.(map[string]any); !isObject {
				t.Errorf("%s %s: property %q is a %T (%v), not an object schema — a strict MCP client rejects a non-object property schema and fails the whole tools/list", toolName, field, name, val, val)
				continue
			}
			walkPropertySchemas(t, toolName, field, val)
		}
	}
	if items, ok := m["items"].(map[string]any); ok {
		walkPropertySchemas(t, toolName, field, items)
	}
	for _, key := range []string{"$defs", "definitions"} {
		if defs, ok := m[key].(map[string]any); ok {
			for _, d := range defs {
				walkPropertySchemas(t, toolName, field, d)
			}
		}
	}
}
