package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

func failingPush(err error) func(ctx context.Context, dr source.DecisionRecord) (recordOutput, error) {
	return func(ctx context.Context, dr source.DecisionRecord) (recordOutput, error) {
		return recordOutput{}, err
	}
}

var errWorkspace404 = errors.New(`record_decision: hosted push failed: import-decisions: HTTP 404: {"error":"workspace not found"}`)

// A hosted push failure must not lose the capture (the #348 lesson: recording
// has to cost nothing at the moment a decision lands). With a local store
// available the recorder preserves the capture as a NON-BINDING local draft and
// says loudly what happened — it does not fail the call, and it does not write
// through Record (a local accepted write would bind a draft nobody accepted).
func TestRecorder_HostedPushFailurePreservesLocalDraft(t *testing.T) {
	fake := &fakeCapture{}
	r := recorder{
		capture:     fake,
		capturePath: ".lema/decisions.jsonl",
		pushHosted:  failingPush(errWorkspace404),
	}

	out, err := r.record(context.Background(), sampleDecisionRecord())
	if err != nil {
		t.Fatalf("fallback must not fail the call: %v", err)
	}
	if !fake.draftCalled {
		t.Fatal("expected the capture to be preserved as a local draft")
	}
	if fake.called {
		t.Fatal("fallback wrote through Record — that binds an unaccepted draft locally")
	}
	if out.Status != "local_draft" {
		t.Fatalf("status = %q, want local_draft", out.Status)
	}
	if out.ID != "d_draft1" {
		t.Fatalf("id = %q, want the local draft id", out.ID)
	}
	for _, want := range []string{
		"hosted push failed",    // lead with the failure — fail loud
		"workspace not found",   // carry the actual cause
		"draft",                 // what the capture landed as
		".lema/decisions.jsonl", // where it landed
		"does not enforce",      // honest about non-binding
		workspaceIDEnv,          // how to fix the mapping
	} {
		if !strings.Contains(out.Recorded, want) {
			t.Fatalf("message missing %q:\n%s", want, out.Recorded)
		}
	}
}

// Without a local store there is nowhere to preserve the capture — the
// original fail-loud behavior stands.
func TestRecorder_HostedPushFailureWithoutLocalStoreStaysError(t *testing.T) {
	r := recorder{pushHosted: failingPush(errWorkspace404)}
	if _, err := r.record(context.Background(), sampleDecisionRecord()); err == nil {
		t.Fatal("no local store to preserve into — the call must fail loud")
	}
}

// If the local draft write ALSO fails, the capture really is lost — the error
// must surface both failures so nothing is silent.
func TestRecorder_HostedFallbackDraftFailureSurfacesBoth(t *testing.T) {
	fake := &fakeCapture{draftErr: errors.New("disk full")}
	r := recorder{capture: fake, pushHosted: failingPush(errWorkspace404)}
	_, err := r.record(context.Background(), sampleDecisionRecord())
	if err == nil {
		t.Fatal("both sinks failed — the call must error")
	}
	for _, want := range []string{"workspace not found", "disk full"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}
