package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
)

// record_decision.go routes an explicit record_decision capture to the right
// backing store. The two modes are deliberately split by the trust tier
// (ADR-0125): in SOLO mode the operator is the only judge, so a capture appends
// to the local .lema/decisions.jsonl store, which the CaptureStore records as
// `accepted` and enforces immediately. In HOSTED mode the capture is pushed to
// the org corpus with NO self-asserted status — the server adjudicates the
// trust tier (captureAcceptFor + soloSelfPush, ADR-0134/0135): a solo owner's
// push auto-accepts into RECALL (if it was implemented, it was decided — no
// accept-queue to scroll); any other programmatic push drafts `proposed`. BIND
// is human either way — the accept event stays actor_kind=agent, so a capture
// can open recall but never a binding ruling (the poison gate). The client must
// not push `proposed` itself: a self-asserted `proposed` never claimed accepted,
// so the server's solo auto-accept can't fire and the capture lands invisible to
// recall (lemahq/lema#355). The local store's accepted path is bypassed in
// hosted mode on purpose: a local `accepted` write would bind a draft on this
// machine that the team has not accepted (and could reject). But a hosted push
// FAILURE must not lose the capture either (the #348 lesson: recording has to
// cost nothing at the moment a decision lands) — so a failed push falls back to
// a local DRAFT (CaptureStore.RecordDraft, status proposed): durable and
// searchable, binding nothing, announced loudly in the tool response. Both
// tenets hold: fail loud, and never lose the write.

// recordPushTimeout bounds the hosted record_decision push from the client side.
// It is longer than the Stop-hook's pushTimeout: record_decision is an
// interactive, awaited capture and the server completes create+ingest
// synchronously (a cold start can exceed 10s), whereas the Stop-hook push must
// stay snappy at turn-end. The server also no longer aborts on a client
// disconnect (importWorkContext), so even a timeout here cannot leave a partial
// capture — it only costs the caller the success ack.
const recordPushTimeout = 30 * time.Second

// decisionRecorder is the active record_decision sink, wired in main() by trust
// tier. The zero value records nothing and errors — main() always sets one.
var decisionRecorder recorder

// captureSink is the local decision store record_decision writes to
// (satisfied by *source.CaptureStore): Record in solo mode, RecordDraft as the
// non-binding preserve-the-write fallback when a hosted push fails. An
// interface so the recorder is testable without a real jsonl file.
type captureSink interface {
	Record(source.DecisionRecord) (source.DecisionRecord, error)
	RecordDraft(source.DecisionRecord) (source.DecisionRecord, error)
}

// recorder is the active sink for record_decision: pushHosted in hosted mode
// (LEMA_API_URL set), capture alone in solo mode. In hosted mode capture is
// ALSO set (when the local store loaded) as the draft fallback for a failed
// push; capturePath names its file for the fallback message.
type recorder struct {
	capture     captureSink
	capturePath string
	targets     targetProvider
	targetInput resolveTargetInput
	pushHosted  func(ctx context.Context, receipt targetContext, dr source.DecisionRecord) (recordOutput, error)
}

type draftTargetEvidenceSource interface {
	DraftTargetEvidence(title, chosen string) (source.TargetEvidence, bool)
}

func newHostedRecorder(runtime hostedWriteRuntime, capture captureSink, capturePath string) recorder {
	return recorder{
		capture:     capture,
		capturePath: capturePath,
		targets:     runtime.targets,
		targetInput: runtime.targetInput,
		pushHosted: func(ctx context.Context, receipt targetContext, dr source.DecisionRecord) (recordOutput, error) {
			push := func(ctx context.Context, records []pushRecord) (pushResponse, error) {
				return pushDecisions(ctx, runtime.client, runtime.apiURL, runtime.token, receipt.RepositoryWorkspaceID, records)
			}
			return recordToHosted(ctx, dr, runtime.timeNow(), push)
		},
	}
}

// record validates the capture and persists it via the active sink. Title and
// chosen are required (mirrors the local CaptureStore guard) so an empty field
// fails the same clean way in hosted mode as in solo, instead of round-tripping
// to a murkier server error. Hosted wins when set; a hosted push failure is
// preserved as a non-binding local draft when the local store is available
// (fail loud, but never lose the write) and stays an error when it is not;
// solo falls through to the local store; a recorder with neither fails loud.
func (r recorder) record(ctx context.Context, dr source.DecisionRecord) (recordOutput, error) {
	if strings.TrimSpace(dr.Title) == "" || strings.TrimSpace(dr.Chosen) == "" {
		return recordOutput{}, fmt.Errorf("title and chosen are required")
	}
	if r.pushHosted != nil {
		if dr.TargetEvidence == nil {
			if drafts, ok := r.capture.(draftTargetEvidenceSource); ok {
				if evidence, found := drafts.DraftTargetEvidence(dr.Title, dr.Chosen); found {
					dr.TargetEvidence = &evidence
				}
			}
		}
		input, inputErr := targetInputForOfflineRetry(r.targetInput, dr.TargetEvidence)
		var receipt targetContext
		var out recordOutput
		var err error
		if inputErr != nil {
			err = inputErr
		} else {
			out, err = withResolvedTarget(ctx, r.targets, input, func(ctx context.Context, resolved targetContext) (recordOutput, error) {
				receipt = resolved
				return r.pushHosted(ctx, resolved, dr)
			})
		}
		if err == nil {
			return out, nil
		}
		if dr.TargetEvidence != nil && targetResolutionStatusFromError(err) == resolutionStale {
			return recordOutput{}, err
		}
		if r.capture == nil {
			return recordOutput{}, err // nowhere to preserve the capture — fail loud
		}
		if validResolvedTargetContext(receipt) {
			dr.TargetEvidence = targetEvidenceFromContext(receipt)
		}
		rec, derr := r.capture.RecordDraft(dr)
		if derr != nil {
			return recordOutput{}, fmt.Errorf("%w; the local draft fallback also failed (%v) — the capture was NOT saved", err, derr)
		}
		path := r.capturePath
		if path == "" {
			path = ".lema/decisions.jsonl"
		}
		return recordOutput{
			ID:     rec.ID,
			Status: "local_draft",
			Recorded: fmt.Sprintf(
				"%v — the capture was preserved locally as a draft in %s (id %s). It surfaces in search but does not enforce, and it is NOT in your team's corpus; run lema-mcp doctor context and fix the target mapping, then record this decision again to revalidate and push it.",
				err, path, rec.ID),
		}, nil
	}
	if r.capture == nil {
		return recordOutput{}, fmt.Errorf("capture store is not available")
	}
	dr.TargetEvidence = nil
	rec, err := r.capture.Record(dr)
	if err != nil {
		return recordOutput{}, err
	}
	msg := fmt.Sprintf("recorded %q → chose %s", rec.Title, rec.Chosen)
	if n := len(rec.Rejected); n > 0 {
		msg += fmt.Sprintf("; %d rejected alternative(s) now CLOSED", n)
	}
	return recordOutput{ID: rec.ID, Status: rec.Status, Recorded: msg}, nil
}

// recordToHosted pushes one capture to the org corpus and maps the server ack
// into the tool output. It carries the full payload — the rejected alternatives
// (the enforcement payload), constraint, consequence, and supersedes — under a
// stable content-keyed id so a re-record is idempotent, and NO self-asserted
// status: the server adjudicates the trust tier (ADR-0134/0135, see the mode
// comment above — a hardcoded `proposed` here is what kept solo captures out of
// recall, lemahq/lema#355). The output status + message follow the server's
// per-record outcome AND the landed current_status it reports: created+accepted
// is live in recall now (a human confirm still binds the ruling); created+
// proposed is a draft whose recall opens on an in-app accept; a created with no
// reported current_status (older server) claims neither; `updated`/`skipped`
// mean the decision already existed (an import never changes a decision's
// lifecycle status), so it does NOT re-announce a fresh capture. Any non-fatal
// server warnings are surfaced so a degraded outcome (e.g. an unresolvable
// supersedes target) is never silent. A transport failure OR a server-reported
// per-record failure is returned (fail loud); the recorder — not this function —
// then preserves the capture as a loud, non-binding local draft (never a silent
// local `accepted` write).
func recordToHosted(ctx context.Context, dr source.DecisionRecord, now time.Time, push func(context.Context, []pushRecord) (pushResponse, error)) (recordOutput, error) {
	rec := pushRecord{
		ID:          sessionDecisionID(dr.Title, dr.Chosen),
		TS:          now.UTC().Format(time.RFC3339),
		Title:       dr.Title,
		Chosen:      dr.Chosen,
		Rejected:    toPushRejected(dr.Rejected),
		Rationale:   dr.Rationale,
		Refs:        dr.Refs,
		Constraint:  dr.Constraint,
		Consequence: dr.Consequence,
		Supersedes:  dr.Supersedes,
	}
	resp, err := push(ctx, []pushRecord{rec})
	if err != nil {
		return recordOutput{}, fmt.Errorf("record_decision: hosted push failed: %w", err)
	}
	if resp.Failed > 0 {
		reason := "the server did not record it"
		if len(resp.Results) > 0 && resp.Results[0].Reason != "" {
			reason = resp.Results[0].Reason
		}
		return recordOutput{}, fmt.Errorf("record_decision: hosted push did not record the decision: %s", reason)
	}

	var r0 pushResult
	if len(resp.Results) > 0 {
		r0 = resp.Results[0]
	}
	id := rec.ID
	if r0.DecisionID != nil && *r0.DecisionID != "" {
		id = *r0.DecisionID
	}

	var status, msg string
	switch r0.Status {
	case "updated":
		status = "updated"
		msg = fmt.Sprintf("updated %q in your team's corpus", dr.Title)
	case "skipped":
		status = "skipped"
		msg = fmt.Sprintf("%q is already recorded in your team's corpus — no change", dr.Title)
	default: // "created", or an empty/unknown status — a new capture
		// Recall honesty rides on the server-reported landed status, never a
		// client-side guess (#355).
		switch r0.CurrentStatus {
		case "accepted":
			status = "accepted"
			msg = fmt.Sprintf("recorded %q in your team's corpus — live in recall now", dr.Title)
			if n := len(dr.Rejected); n > 0 {
				msg += fmt.Sprintf(" (its %d ruled-out alternative(s) start enforcing once a human confirms the ruling — an agent capture never self-binds)", n)
			}
		case pushStatusProposed:
			status = pushStatusProposed
			msg = fmt.Sprintf("drafted %q as proposed in your team's corpus — a human accepts it in-app to open recall and bind its ruling", dr.Title)
		default: // older server: landed status unreported — claim neither recall nor draft
			status = "recorded"
			msg = fmt.Sprintf("recorded %q in your team's corpus", dr.Title)
			if n := len(dr.Rejected); n > 0 {
				msg += fmt.Sprintf(" (%d rejected alternative(s) recorded — a human accept in-app is what binds them)", n)
			}
		}
	}
	if len(r0.Warnings) > 0 {
		msg += " — note: " + strings.Join(r0.Warnings, "; ")
	}
	return recordOutput{ID: id, Status: status, Recorded: msg}, nil
}

// toPushRejected maps capture rejected alternatives onto the push wire shape.
func toPushRejected(alts []source.RejectedAlt) []pushRejectedAlt {
	if len(alts) == 0 {
		return nil
	}
	out := make([]pushRejectedAlt, len(alts))
	for i, a := range alts {
		out[i] = pushRejectedAlt{Option: a.Option, Why: a.Why}
	}
	return out
}

// toDecisionRecord projects the MCP record_decision input onto the canonical
// capture model the recorder operates on (shared with the GUI httpRecord path),
// so both surfaces map the same fields in one place rather than two that can drift.
func (in recordInput) toDecisionRecord() source.DecisionRecord {
	return source.DecisionRecord{
		Title: in.Title, Chosen: in.Chosen, Rejected: in.Rejected, Rationale: in.Rationale,
		Refs: in.Refs, Constraint: in.Constraint, Consequence: in.Consequence, Supersedes: in.Supersedes,
	}
}
