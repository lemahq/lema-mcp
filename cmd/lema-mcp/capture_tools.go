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
	Assent      string               `json:"assent,omitempty" jsonschema:"only when the operator explicitly affirmed this decision in-session: their affirmation, quoted or paraphrased with date — stages the capture for one-keystroke boundary binding; never binds by itself"`
}

type recordOutput struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Recorded string `json:"recorded"`
}

// recordDecision captures a decision at deliberation. The calling agent forms the
// atom (bring-your-own-AI); the recorder persists it via the active sink (the
// local capture store in solo mode, or a proposed draft to the org corpus in
// hosted mode — record_decision.go). The sink is wired in main() by trust tier.
func recordDecision(ctx context.Context, _ *mcp.CallToolRequest, in recordInput) (*mcp.CallToolResult, recordOutput, error) {
	out, err := decisionRecorder.record(ctx, in.toDecisionRecord())
	if err != nil {
		return nil, recordOutput{}, err
	}
	logUsage("record_decision", in.Title, 1, in)
	return nil, out, nil
}

type checkInput struct {
	Topic string `json:"topic" jsonschema:"the direction or option you are about to propose — checked against decisions already settled and closed"`
	// WorkspaceIDs can only narrow a hosted check within the repository leaves
	// named by the resolved Project receipt. Omission uses that complete set.
	WorkspaceIDs []string `json:"workspace_ids,omitempty" jsonschema:"workspace ids may only narrow within the resolved Project repository set; omit to use the complete resolved Project repository set"`
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
	out, err := checkDecidedFor(ctx, in.Topic, in.WorkspaceIDs)
	if err != nil {
		return nil, out, fmt.Errorf("check_decided: %w", err)
	}
	logUsage("check_decided", in.Topic, len(out.Closed), out)
	return nil, out, nil
}

// checkDecidedFor is the ONE acquisition-and-judgment path behind every check
// surface: the stdio check_decided tool above and the GUI's GET /api/check
// (serve.go) both call it, so the two doors cannot diverge. (They did: the
// lema-terminal design debate caught /api/check answering from a local-lexical
// capture.CheckDecided — no repo-ADR set, no hosted closures, no ADR-0094
// verdict envelope — while the tool judged the full merged set via
// verdict.Build; the same topic got two different answers. Its design-lock
// ruling: one adjudicator, one matcher, at the boundary.)
//
// It enforces off BOTH the capture store and the repo's documented ADRs
// (ADR-0053), matched by the distinctiveness-weighted matcher (ADR-0053 recall
// calibration): the old all-tokens-AND rule required a query to contain an
// option's entire (often full-sentence) name, so recall on natural-language
// topics was ~zero. The weighted matcher fires when the query names the
// option's distinctive terms, holding precision via the stopword prior
// (generic words never anchor a match). NB: this is the natural-language check
// path; the PreToolUse guard hook still uses the token matcher (guardMatch)
// over code-edit text, which is a different input distribution awaiting its
// own eval.
//
// Hosted mode (build-plan D.1): pull the org's CLOSED set from the hosted
// graph — rejected alternatives of accepted, non-superseded decisions — so the
// check enforces the TEAM's record, not just this machine's capture file. A
// fetch failure FAILS the call with the errored envelope: before this leg,
// hosted check_decided silently checked local capture only, and a silent
// degrade back to that is worse than a visible, retryable error (ADR-0094).
func checkDecidedFor(ctx context.Context, topic string, workspaceIDs []string) (checkOutput, error) {
	merged := []source.Atom{}
	if capture != nil {
		merged = append(merged, capture.ClosedAtoms()...)
	}
	if cs, ok := src.(source.ClosedSource); ok {
		merged = append(merged, cs.ClosedAtoms()...)
	}
	if cf, ok := src.(source.ClosedFetcher); ok {
		runtime, err := currentHostedRuntime()
		if err != nil {
			ev := verdict.NewErrored("hosted closed-decision fetch failed; not answering from local capture alone")
			out := checkOutput{Topic: topic, Verdict: string(ev.Verdict), GoverningDecisions: ev.GoverningDecisions, Reason: ev.Reason}
			return out, err
		}
		hostedClosed, err := withHostedReadScope(ctx, runtime, workspaceIDs, func(ctx context.Context, scope []string, _ targetContext) ([]source.Atom, error) {
			return cf.FetchClosedAtoms(ctx, scope)
		})
		if err != nil {
			ev := verdict.NewErrored("hosted closed-decision fetch failed; not answering from local capture alone")
			out := checkOutput{Topic: topic, Verdict: string(ev.Verdict), GoverningDecisions: ev.GoverningDecisions, Reason: ev.Reason}
			return out, fmt.Errorf("hosted closed-decision fetch failed: %w", err)
		}
		merged = append(merged, hostedClosed...)
	}
	return buildCheckOutput(topic, merged), nil
}

// checkClosedCap bounds the atoms surfaced in `closed`. check_decided ADJUDICATES,
// it does not enumerate the corpus; the structural set is already small, but a cap
// makes it impossible for the output to balloon the way the raw lexical Match once
// did (a 53-atom, 63KB dump on a vocabulary coincidence).
const checkClosedCap = 8

// buildCheckOutput is the pure happy-path builder: the legacy fields plus the
// verdict envelope (ADR-0094), judged over an acquired CLOSED set by the shared
// verdict.Build so the MCP and proposemode surfaces render the SAME judgment.
func buildCheckOutput(topic string, merged []source.Atom) checkOutput {
	v := verdict.Build(merged, topic)
	// `closed` mirrors the verdict: the STRUCTURALLY governing atoms (verdict.Governing),
	// not every lexical Match hit — so the legacy decided/closed/note fields can never
	// contradict the verdict (a not_ruled_out that still says "decided:true, do not
	// re-propose" over dozens of vocabulary-collision atoms was the reported bug).
	governing := verdict.Governing(merged, topic)
	total := len(governing)
	closed := governing
	if len(closed) > checkClosedCap {
		closed = closed[:checkClosedCap]
	}
	out := checkOutput{
		Topic:              topic,
		Decided:            total > 0,
		Closed:             closed,
		Verdict:            string(v.Verdict),
		GoverningDecisions: v.GoverningDecisions,
		Reason:             v.Reason,
	}
	if out.Decided {
		out.Note = "this topic touches decisions already CLOSED — do not re-propose the closed options; surface the prior decision instead"
		if total > checkClosedCap {
			out.Note += fmt.Sprintf(" (showing the %d most relevant of %d)", checkClosedCap, total)
		}
	}
	return out
}
