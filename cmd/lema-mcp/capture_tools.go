package main

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lemahq/lema-mcp/internal/source"
	"github.com/lemahq/lema-mcp/internal/verdict"
)

type recordInput struct {
	Title       string               `json:"title" jsonschema:"the decision topic, e.g. 'state management for the web app'"`
	Chosen      string               `json:"chosen" jsonschema:"what you decided — the option you are going with"`
	Rejected    []source.RejectedAlt `json:"rejected,omitempty" jsonschema:"the alternatives you considered and rejected, each with why it was killed — record these; they are what stops the team and future agents from re-proposing a dead end"`
	Rationale   string               `json:"rationale,omitempty" jsonschema:"why the chosen option won"`
	Refs        []string             `json:"refs,omitempty" jsonschema:"files, ADRs, or PRs this decision touches"`
	Constraint  string               `json:"constraint,omitempty" jsonschema:"a hard limit this decision imposes"`
	Consequence string               `json:"consequence,omitempty" jsonschema:"a downstream effect of this decision"`
	Supersedes  []string             `json:"supersedes,omitempty" jsonschema:"ids of earlier decisions this overrides; they become CLOSED (never reopen)"`
}

type recordOutput struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Recorded string `json:"recorded"`
}

// recordDecision captures a decision at deliberation to the local store. The
// calling agent forms the atom (bring-your-own-AI); the server only persists it.
func recordDecision(_ context.Context, _ *mcp.CallToolRequest, in recordInput) (*mcp.CallToolResult, recordOutput, error) {
	if capture == nil {
		return nil, recordOutput{}, fmt.Errorf("capture store is not available")
	}
	rec, err := capture.Record(source.DecisionRecord{
		Title: in.Title, Chosen: in.Chosen, Rejected: in.Rejected, Rationale: in.Rationale,
		Refs: in.Refs, Constraint: in.Constraint, Consequence: in.Consequence, Supersedes: in.Supersedes,
	})
	if err != nil {
		return nil, recordOutput{}, err
	}
	msg := fmt.Sprintf("recorded %q → chose %s", rec.Title, rec.Chosen)
	if n := len(rec.Rejected); n > 0 {
		msg += fmt.Sprintf("; %d rejected alternative(s) now CLOSED", n)
	}
	logUsage("record_decision", rec.Title, 1, rec)
	return nil, recordOutput{ID: rec.ID, Status: rec.Status, Recorded: msg}, nil
}

type checkInput struct {
	Topic string `json:"topic" jsonschema:"the direction or option you are about to propose — checked against decisions already settled and closed"`
	// WorkspaceIDs optionally scopes the check to specific workspaces. Omit to
	// check every workspace you can see; pass the repo's own workspace so a check
	// never trips on an unrelated repo's rejected option (cross-repo false ruled_out).
	WorkspaceIDs []string `json:"workspace_ids,omitempty" jsonschema:"optional workspace ids to scope the check to; omit to check every workspace you can see"`
}

type checkOutput struct {
	Topic   string `json:"topic"`
	Decided bool   `json:"decided"`
	// source.Atom is embedded directly, so additive Atom fields (locator, refs)
	// ride through check_decided automatically. These atoms get NO trust prefix,
	// so their refs are sanitized at capture time (source.sanitizeRefs).
	Closed []source.Atom `json:"closed"`
	Note   string        `json:"note,omitempty"`
	// Verdict envelope (ADR-0094) — additive. New callers read these; `decided`
	// and `closed` stay for back-compat and may differ from `verdict` (decided =
	// any closed match; verdict = the refined judgment — ruled_out only on a
	// binding match, incomplete/error when the closed set can't be trusted).
	Verdict            string                      `json:"verdict"`
	GoverningDecisions []verdict.GoverningDecision `json:"governing_decisions"`
	Reason             string                      `json:"reason"`
}

// checkDecided is the never-reopen gate: before proposing a direction, an agent
// calls this and, if anything comes back CLOSED, surfaces the prior decision
// instead of re-proposing the dead option.
func checkDecided(ctx context.Context, _ *mcp.CallToolRequest, in checkInput) (*mcp.CallToolResult, checkOutput, error) {
	if capture == nil {
		return nil, checkOutput{Topic: in.Topic}, nil
	}
	// Enforce off BOTH the capture store and the repo's documented ADRs (ADR-0053),
	// matched by the distinctiveness-weighted matcher (ADR-0053 recall calibration):
	// the old all-tokens-AND rule required a query to contain an option's entire
	// (often full-sentence) name, so recall on natural-language topics was ~zero.
	// The weighted matcher fires when the query names the option's distinctive
	// terms, holding precision via the stopword prior (generic words never anchor a
	// match). NB: this is the natural-language check_decided path; the PreToolUse
	// guard hook still uses the token matcher (guardMatch) over code-edit text,
	// which is a different input distribution awaiting its own eval.
	merged := append([]source.Atom{}, capture.ClosedAtoms()...)
	if cs, ok := src.(source.ClosedSource); ok {
		merged = append(merged, cs.ClosedAtoms()...)
	}
	// Hosted mode (build-plan D.1): pull the org's CLOSED set from the hosted
	// graph — rejected alternatives of accepted, non-superseded decisions —
	// so check_decided enforces the TEAM's record, not just this machine's
	// capture file. A fetch failure FAILS the tool call: before this leg,
	// hosted check_decided silently checked local capture only, and a silent
	// degrade back to that is worse than a visible, retryable error.
	if cf, ok := src.(source.ClosedFetcher); ok {
		hostedClosed, err := cf.FetchClosedAtoms(ctx, in.WorkspaceIDs)
		if err != nil {
			// Fail loud: a fetch failure returns an ERROR verdict, never a confident
			// answer from local capture alone (ADR-0094 / the pre-existing contract).
			ev := verdict.NewErrored("hosted closed-decision fetch failed; not answering from local capture alone")
			out := checkOutput{Topic: in.Topic, Verdict: string(ev.Verdict), GoverningDecisions: ev.GoverningDecisions, Reason: ev.Reason}
			return nil, out, fmt.Errorf("check_decided: hosted closed-decision fetch failed: %w", err)
		}
		merged = append(merged, hostedClosed...)
	}
	out := buildCheckOutput(in.Topic, merged)
	logUsage("check_decided", in.Topic, len(out.Closed), out)
	return nil, out, nil
}

// buildCheckOutput is the pure happy-path builder: the legacy fields plus the
// verdict envelope (ADR-0094), judged over an acquired CLOSED set by the shared
// verdict.Build so the MCP and proposemode surfaces render the SAME judgment.
func buildCheckOutput(topic string, merged []source.Atom) checkOutput {
	v := verdict.Build(merged, topic)
	closed := verdict.Match(merged, topic, verdict.MatchThreshold)
	out := checkOutput{
		Topic:              topic,
		Decided:            len(closed) > 0,
		Closed:             closed,
		Verdict:            string(v.Verdict),
		GoverningDecisions: v.GoverningDecisions,
		Reason:             v.Reason,
	}
	if out.Decided {
		out.Note = "this topic touches decisions already CLOSED — do not re-propose the closed options; surface the prior decision instead"
	}
	return out
}
