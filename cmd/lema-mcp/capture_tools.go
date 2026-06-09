package main

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lemahq/lema-mcp/internal/source"
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
}

type checkOutput struct {
	Topic   string `json:"topic"`
	Decided bool   `json:"decided"`
	// source.Atom is embedded directly, so additive Atom fields (locator, refs)
	// ride through check_decided automatically. These atoms get NO trust prefix,
	// so their refs are sanitized at capture time (source.sanitizeRefs).
	Closed []source.Atom `json:"closed"`
	Note   string        `json:"note,omitempty"`
}

// checkDecided is the never-reopen gate: before proposing a direction, an agent
// calls this and, if anything comes back CLOSED, surfaces the prior decision
// instead of re-proposing the dead option.
func checkDecided(_ context.Context, _ *mcp.CallToolRequest, in checkInput) (*mcp.CallToolResult, checkOutput, error) {
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
	closed := weightedGuardMatch(merged, in.Topic, guardMatchThreshold)
	out := checkOutput{Topic: in.Topic, Decided: len(closed) > 0, Closed: closed}
	if out.Decided {
		out.Note = "this topic touches decisions already CLOSED — do not re-propose the closed options; surface the prior decision instead"
	}
	logUsage("check_decided", in.Topic, len(closed), out)
	return nil, out, nil
}
