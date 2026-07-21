package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// propose is a router, not a re-implementation: it must reach the SAME
// recorder path record_decision uses, so the verb and its alias can never
// persist different captures during the deprecation window. These tests pin
// that routing on both trust tiers plus the shared validation.

// setSoloRecorder points decisionRecorder at a fresh solo-tier capture store
// (local JSONL append, no hosted push) and restores the prior recorder.
func setSoloRecorder(t *testing.T) *source.CaptureStore {
	t.Helper()
	prev := decisionRecorder
	t.Cleanup(func() { decisionRecorder = prev })

	cs, err := source.NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatalf("NewCaptureStore: %v", err)
	}
	decisionRecorder = recorder{capture: cs}
	return cs
}

// Solo tier (d_5b49e6): the capture appends to the local store as accepted —
// recall opens for the solo operator immediately; nothing binds.
func TestProposeSoloAppendsAccepted(t *testing.T) {
	cs := setSoloRecorder(t)
	_, out, err := propose(context.Background(), nil, proposeInput{
		Title:    "queue backend",
		Chosen:   "Postgres SKIP LOCKED",
		Rejected: []source.RejectedAlt{{Option: "Redis streams", Why: "second datastore to operate"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "accepted" {
		t.Fatalf("solo propose must land the trust-tier accepted append, got status %q", out.Status)
	}
	if !strings.Contains(out.Recorded, "CLOSED") {
		t.Errorf("solo message should report the rejected alternative now CLOSED: %q", out.Recorded)
	}
	atoms := cs.ClosedAtoms()
	if len(atoms) != 1 || !strings.Contains(atoms[0].Text, "Redis streams") {
		t.Fatalf("the rejected alternative must be in the store's closed set: %+v", atoms)
	}
}

// Hosted tier: propose must route through the recorder's hosted push — the
// server adjudicates the landed status; the client asserts none.
func TestProposeHostedRoutesThroughRecorderPush(t *testing.T) {
	prev := decisionRecorder
	t.Cleanup(func() { decisionRecorder = prev })

	var got source.DecisionRecord
	decisionRecorder = recorder{pushHosted: func(_ context.Context, dr source.DecisionRecord) (recordOutput, error) {
		got = dr
		return recordOutput{ID: "dec_9", Status: "proposed", Recorded: "drafted"}, nil
	}}

	_, out, err := propose(context.Background(), nil, proposeInput{
		Title: "t", Chosen: "c", Supersedes: []string{"d_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "t" || got.Chosen != "c" || len(got.Supersedes) != 1 {
		t.Fatalf("the full capture payload must reach the hosted push: %+v", got)
	}
	if out.Status != "proposed" || out.ID != "dec_9" {
		t.Fatalf("the server-adjudicated outcome must ride through verbatim: %+v", out)
	}
}

// A failed hosted push must surface record_decision's loud local-draft
// fallback through propose unchanged — never a lost capture.
func TestProposeHostedFailureFallsBackToLocalDraft(t *testing.T) {
	prev := decisionRecorder
	t.Cleanup(func() { decisionRecorder = prev })

	cs, err := source.NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatalf("NewCaptureStore: %v", err)
	}
	decisionRecorder = recorder{
		capture: cs,
		pushHosted: func(_ context.Context, _ source.DecisionRecord) (recordOutput, error) {
			return recordOutput{}, errors.New("api unreachable")
		},
	}

	_, out, err := propose(context.Background(), nil, proposeInput{Title: "t", Chosen: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "local_draft" || !strings.Contains(out.Recorded, "NOT in your team's corpus") {
		t.Fatalf("a failed push must be preserved as the loud non-binding draft: %+v", out)
	}
}

// The alias-then-deprecate drift pin: the same input through propose and
// through record_decision must produce the identical persisted outcome
// (content-keyed id, status, message) — one recorder, one contract.
func TestProposeAndAliasPersistIdenticalCaptures(t *testing.T) {
	in := proposeInput{
		Title:    "retry policy",
		Chosen:   "exponential backoff",
		Rejected: []source.RejectedAlt{{Option: "fixed interval", Why: "thundering herd"}},
	}

	setSoloRecorder(t)
	_, viaPropose, err := propose(context.Background(), nil, in)
	if err != nil {
		t.Fatal(err)
	}

	setSoloRecorder(t) // fresh store, so the alias writes the same first append
	_, viaAlias, err := recordDecision(context.Background(), nil, in)
	if err != nil {
		t.Fatal(err)
	}

	if viaPropose != viaAlias {
		t.Fatalf("propose and record_decision must persist identically:\n propose: %+v\n   alias: %+v", viaPropose, viaAlias)
	}
}

// Validation is the recorder's, shared with the alias: an empty required
// field fails the same clean way.
func TestProposeValidatesRequiredFields(t *testing.T) {
	setSoloRecorder(t)
	if _, _, err := propose(context.Background(), nil, proposeInput{Title: "t"}); err == nil {
		t.Fatal("propose without chosen must error like record_decision does")
	}
}
