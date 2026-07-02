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
// `accepted` and enforces immediately. In HOSTED mode the team is the judge, so
// a capture is pushed to the org corpus as a `proposed` draft (server-coerced —
// a programmatic principal can never self-bind) and a human's later in-app accept
// is what binds it. The local store is bypassed in hosted mode on purpose: a
// local `accepted` write would bind a draft on this machine that the team has not
// accepted (and could reject), the exact poison the proposed gate prevents.

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

// captureSink is the local decision store record_decision writes to in solo mode
// (satisfied by *source.CaptureStore). An interface so the recorder is testable
// without a real jsonl file.
type captureSink interface {
	Record(source.DecisionRecord) (source.DecisionRecord, error)
}

// recorder is the active sink for record_decision. Exactly one of the two sinks
// is set: pushHosted in hosted mode (LEMA_API_URL set), capture in solo mode.
type recorder struct {
	capture    captureSink
	pushHosted func(ctx context.Context, dr source.DecisionRecord) (recordOutput, error)
}

// record validates the capture and persists it via the active sink. Title and
// chosen are required (mirrors the local CaptureStore guard) so an empty field
// fails the same clean way in hosted mode as in solo, instead of round-tripping
// to a murkier server error. Hosted wins when set; solo falls through to the
// local store; a recorder with neither fails loud.
func (r recorder) record(ctx context.Context, dr source.DecisionRecord) (recordOutput, error) {
	if strings.TrimSpace(dr.Title) == "" || strings.TrimSpace(dr.Chosen) == "" {
		return recordOutput{}, fmt.Errorf("title and chosen are required")
	}
	if r.pushHosted != nil {
		return r.pushHosted(ctx, dr)
	}
	if r.capture == nil {
		return recordOutput{}, fmt.Errorf("capture store is not available")
	}
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

// recordToHosted pushes one capture to the org corpus as a single PROPOSED draft
// and maps the server ack into the tool output. It carries the full payload — the
// rejected alternatives (the enforcement payload), constraint, consequence, and
// supersedes — under a stable content-keyed id so a re-record is idempotent. The
// output status + message follow the server's per-record outcome: `created` is a
// new proposed draft — live in recall, with a human confirm what binds its
// ruling (per ADR-0135 there is no inbox accept-queue); `updated`/`skipped` mean
// the decision already existed (an import never changes a decision's lifecycle
// status), so it does NOT re-announce a fresh draft. Any non-fatal server warnings are surfaced so
// a degraded outcome (e.g. an unresolvable supersedes target) is never silent. A
// transport failure OR a server-reported per-record failure is returned (fail
// loud); it NEVER silently falls back to the local store.
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
		Status:      pushStatusProposed,
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
	default: // "created", or an empty/unknown status — treat as a new proposed draft
		status = pushStatusProposed
		msg = fmt.Sprintf("recorded %q in your team's corpus — live in recall now", dr.Title)
		if n := len(dr.Rejected); n > 0 {
			msg += fmt.Sprintf(" (its %d ruled-out alternative(s) start enforcing once a human confirms the ruling — an agent capture never self-binds)", n)
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
