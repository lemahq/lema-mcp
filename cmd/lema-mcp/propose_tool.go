// propose_tool.go is the `propose` MCP verb (pivot B2, PIVOT_SPEC §4): the
// write verb of the three-verb end state (get_state_brief · resolve ·
// propose) — enqueue one decision candidate for adjudication. The third and
// last end-state verb to land, after get_state_brief and resolve.
//
// Alias-then-deprecate posture (ADR-0110 precedent; the same move resolve
// shipped for the reads): propose does not re-implement the capture path — it
// ROUTES to the record_decision handler verbatim, so the trust-tier semantics
// can never drift from the alias during the deprecation window
// (record_decision.go carries the full contract):
//
//   - solo tier: the capture appends to the local .lema/decisions.jsonl store
//     as accepted — the operator is the only judge, so a capture opens recall
//     immediately; BIND never — binding requires a human ruling (ADR-0125).
//   - hosted tier: the capture pushes with NO self-asserted status and the
//     SERVER adjudicates the trust tier (the capture-authority ruling
//     d_5b49e6 / V15): a solo owner's push auto-accepts into recall, any
//     other programmatic push lands `proposed` pending a human accept. A
//     failed push falls back to a loud, non-binding local draft — never a
//     lost capture, never a silent local accept.
//
// A supersession candidate rides the same payload (`supersedes`); the
// checkpoint-anchored claim kind (HC-6) is NOT stubbed here — it ships only
// when its checkpoint anchor exists to attach to.
package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// proposeInput is a type ALIAS for the record_decision input, not a copy: the
// two schemas are one type, so the verb and its alias cannot drift apart
// field-by-field during the deprecation window.
type proposeInput = recordInput

var proposeTool = &mcp.Tool{
	Name: "propose",
	Description: "Proposes one decision candidate into your team's decision record: the chosen option and the " +
		"rejected alternatives (with why each was killed), plus optional rationale, refs, constraint, consequence, " +
		"and ids of decisions it supersedes. Running solo, the proposal is recorded to this repo's local decision " +
		"store; connected to a hosted workspace, it is pushed for server-side adjudication — the trust tier decides " +
		"whether it lands live in recall or as a proposed draft, and a human ruling is what binds it either way. " +
		"Appends only — it never deletes or overwrites existing decisions. The single write verb over the record; " +
		"record_decision remains registered as its alias.",
	// A writer, not a read: additive appends/pushes only, never a hard delete.
	Annotations: writerLocal("Propose a decision"),
}

// propose enqueues one decision candidate via the record_decision handler —
// the alias-then-deprecate guarantee that propose and its alias persist the
// exact same capture through the exact same recorder. A propose-level
// logUsage rides alongside the handler's own log so the deprecation window
// can watch the new verb's adoption against the alias it will replace.
func propose(ctx context.Context, req *mcp.CallToolRequest, in proposeInput) (*mcp.CallToolResult, recordOutput, error) {
	logUsage("propose", in.Title, 1, in)
	return recordDecision(ctx, req, in)
}
