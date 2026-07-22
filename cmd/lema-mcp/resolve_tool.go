// resolve_tool.go is the `resolve` MCP verb (pivot B2, PIVOT_SPEC §4): one
// cited read over the record with a DECLARED INTENT, the second of the three
// end-state verbs to land after get_state_brief.
//
//   - why-mode      → a cited synthesis (or an honest abstain) over the hosted
//     decision graph. Hosted-only: the DB-less/LLM-free wedge
//     binary cannot synthesize, so without hosted config this
//     mode returns an honest note pointing at approach/id mode.
//   - approach-mode → a typed verdict on ONE option (ruled_out / not_ruled_out
//     / incomplete) with the governing decision and its why,
//     adjudicated over the team's OWN record.
//   - id-mode       → one record's detail by number, or ranked claims for a
//     natural-language query (the discovery front of the same
//     read).
//
// Alias-then-deprecate posture (ADR-0110 precedent; the same move that folded
// why_decided/settled/why_not_public into check_approach in 0.12.0): every
// existing tool — ask, check_decided, get_decision, search_decisions,
// get_decision_graph, list_decisions — stays registered and unchanged. resolve
// does not re-implement any of them; it ROUTES to the exact same handler for
// the declared intent, so the verdict, synthesis, and detail can never drift
// from their single-tool aliases during the deprecation window. The aliases
// carry list/graph until those reads fold into id-mode in a follow-up.
package main

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type resolveInput struct {
	Intent       string   `json:"intent" jsonschema:"the kind of read: 'why' for a cited synthesis, 'approach' for a typed verdict on one option, 'id' for one record's detail or a ranked search"`
	Query        string   `json:"query,omitempty" jsonschema:"why-mode: the natural-language question; id-mode: the search query when no number is given"`
	Approach     string   `json:"approach,omitempty" jsonschema:"approach-mode: the option, library, pattern, or design being weighed against the recorded rejections"`
	Number       int      `json:"number,omitempty" jsonschema:"id-mode: an ADR number to fetch one record's detail; omit to search by query instead"`
	WorkspaceIDs []string `json:"workspace_ids,omitempty" jsonschema:"optional workspace ids to scope the read to; omit to read every workspace you can see"`
}

// resolveOutput is the unified envelope. Exactly one mode payload is populated
// per call (the one matching Intent); the others stay nil so a reader keys off
// Intent. Note carries the honest-abstain / misconfiguration message when a
// mode cannot serve — never a fabricated answer.
type resolveOutput struct {
	Intent   string        `json:"intent"`
	Note     string        `json:"note,omitempty"`
	Why      *askOutput    `json:"why,omitempty"`
	Approach *checkOutput  `json:"approach,omitempty"`
	Record   *getOutput    `json:"record,omitempty"`
	Search   *searchOutput `json:"search,omitempty"`
}

var resolveTool = &mcp.Tool{
	Name: "resolve",
	Description: "Returns one cited read over your team's recorded decisions under a declared intent. " +
		"why-mode returns a synthesized, cited answer to a question (and abstains plainly when the record is silent); " +
		"approach-mode returns a typed verdict on one option — ruled_out when a binding prior decision governs it, " +
		"not_ruled_out otherwise — with the governing decision and its recorded why; " +
		"id-mode returns one decision's full detail by ADR number, or the most relevant claims for a natural-language query. " +
		"The single read over the record; the mode-specific tools remain as aliases.",
	// Open-world: why-mode reaches the hosted API. approach/id also serve over
	// the local record, but the network reach makes external the honest hint.
	Annotations: readOnlyExternal("Resolve a question against your decision record"),
}

// resolve routes to the intent's canonical handler. It re-uses the existing
// tool functions verbatim — the alias-then-deprecate guarantee that resolve
// and its aliases return identical judgments. A resolve-level logUsage rides
// alongside each handler's own log so the deprecation window can watch the new
// verb's adoption against the aliases it will replace.
func resolve(ctx context.Context, req *mcp.CallToolRequest, in resolveInput) (*mcp.CallToolResult, resolveOutput, error) {
	intent := strings.ToLower(strings.TrimSpace(in.Intent))
	logUsage("resolve", intent, 1, in)
	switch intent {
	case "why":
		if hostedSrc == nil {
			return nil, resolveOutput{
				Intent: "why",
				Note:   "why-mode is hosted-only: the cited synthesis needs the hosted decision graph. Run lema-mcp with LEMA_API_URL set, or read the local record with intent=approach or intent=id.",
			}, nil
		}
		_, a, err := askHosted(ctx, req, askInput{Query: in.Query, WorkspaceIDs: in.WorkspaceIDs})
		if err != nil {
			return nil, resolveOutput{Intent: "why"}, err
		}
		return nil, resolveOutput{Intent: "why", Why: &a}, nil

	case "approach":
		topic := strings.TrimSpace(in.Approach)
		if topic == "" {
			topic = strings.TrimSpace(in.Query)
		}
		_, c, err := checkDecided(ctx, req, checkInput{Topic: topic, WorkspaceIDs: in.WorkspaceIDs})
		if err != nil {
			return nil, resolveOutput{Intent: "approach"}, err
		}
		return nil, resolveOutput{Intent: "approach", Approach: &c}, nil

	case "id":
		if in.Number > 0 {
			_, g, err := getDecision(ctx, req, getInput{Number: in.Number})
			if err != nil {
				return nil, resolveOutput{Intent: "id"}, err
			}
			return nil, resolveOutput{Intent: "id", Record: &g}, nil
		}
		_, s, err := searchDecisions(ctx, req, searchInput{Query: in.Query, WorkspaceIDs: in.WorkspaceIDs})
		if err != nil {
			return nil, resolveOutput{Intent: "id"}, err
		}
		return nil, resolveOutput{Intent: "id", Search: &s}, nil

	default:
		return nil, resolveOutput{
			Intent: intent,
			Note:   "declare an intent: 'why' (cited synthesis), 'approach' (typed verdict on one option), or 'id' (one record's detail by number, or a search query).",
		}, nil
	}
}
