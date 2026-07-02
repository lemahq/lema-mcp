package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
)

// fakeCapture is a captureSink that records the input it was handed and mimics
// the real CaptureStore's contract: it stamps a content id and forces accepted.
type fakeCapture struct {
	got    source.DecisionRecord
	called bool
	err    error
}

func (f *fakeCapture) Record(in source.DecisionRecord) (source.DecisionRecord, error) {
	f.called = true
	f.got = in
	if f.err != nil {
		return source.DecisionRecord{}, f.err
	}
	in.ID = "d_fake01"
	in.Status = "accepted"
	return in, nil
}

func sampleDecisionRecord() source.DecisionRecord {
	return source.DecisionRecord{
		Title:       "state management for the web app",
		Chosen:      "Zustand",
		Rejected:    []source.RejectedAlt{{Option: "Redux", Why: "boilerplate"}, {Option: "MobX", Why: "too much magic"}},
		Rationale:   "minimal API, no provider tree",
		Refs:        []string{"apps/web/store.ts", "ADR-0099"},
		Constraint:  "no new top-level providers",
		Consequence: "stores colocate with their feature",
		Supersedes:  []string{"d_abc123"},
	}
}

// In hosted mode a capture is pushed as a single PROPOSED draft carrying the full
// record_decision payload (rejected alts with why, constraint, consequence,
// supersedes), a stable content-keyed id, and the stamped time — and the tool
// output conveys it's live in recall, with a human confirm what binds the ruling
// (no inbox accept-queue, ADR-0135). The whole reason hosted capture is safe: the
// draft can only ever be proposed; a human confirm (never the agent) is what binds.
func TestRecordToHosted_DraftsFullProposedRecord(t *testing.T) {
	dr := sampleDecisionRecord()
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)

	var got []pushRecord
	push := func(_ context.Context, recs []pushRecord) (pushResponse, error) {
		got = recs
		decID := "dec_9001"
		return pushResponse{
			Created:    1,
			RecordedBy: "agent",
			Results:    []pushResult{{LocalID: recs[0].ID, Title: recs[0].Title, Status: "created", DecisionID: &decID}},
		}, nil
	}

	out, err := recordToHosted(context.Background(), dr, now, push)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("want exactly one pushed record, got %d: %+v", len(got), got)
	}
	r := got[0]
	if r.Status != "proposed" {
		t.Errorf("status = %q, want proposed (a programmatic capture must never self-bind)", r.Status)
	}
	if r.ID != sessionDecisionID(dr.Title, dr.Chosen) {
		t.Errorf("id = %q, want the content-keyed id %q (idempotent re-record)", r.ID, sessionDecisionID(dr.Title, dr.Chosen))
	}
	if r.TS != "2026-06-27T12:00:00Z" {
		t.Errorf("ts = %q, want the stamped RFC3339 time", r.TS)
	}
	if r.Title != dr.Title || r.Chosen != dr.Chosen || r.Rationale != dr.Rationale {
		t.Errorf("title/chosen/rationale not mapped: %+v", r)
	}
	if r.Constraint != dr.Constraint || r.Consequence != dr.Consequence {
		t.Errorf("constraint/consequence not mapped: %+v", r)
	}
	if !reflect.DeepEqual(r.Refs, dr.Refs) {
		t.Errorf("refs = %v, want %v", r.Refs, dr.Refs)
	}
	if !reflect.DeepEqual(r.Supersedes, dr.Supersedes) {
		t.Errorf("supersedes = %v, want %v", r.Supersedes, dr.Supersedes)
	}
	wantRej := []pushRejectedAlt{{Option: "Redux", Why: "boilerplate"}, {Option: "MobX", Why: "too much magic"}}
	if !reflect.DeepEqual(r.Rejected, wantRej) {
		t.Errorf("rejected = %+v, want %+v (the killed options are the enforcement payload)", r.Rejected, wantRej)
	}

	if out.Status != "proposed" {
		t.Errorf("output status = %q, want proposed", out.Status)
	}
	if out.ID != "dec_9001" {
		t.Errorf("output id = %q, want the server decision id dec_9001", out.ID)
	}
	if !strings.Contains(strings.ToLower(out.Recorded), "confirm") {
		t.Errorf("output message %q must convey that a human confirm binds the ruling", out.Recorded)
	}
	if strings.Contains(strings.ToLower(out.Recorded), "inbox") {
		t.Errorf("output message %q must not reference the removed inbox (ADR-0135)", out.Recorded)
	}
}

// #4: the output status + message follow the server's per-record outcome. A
// created record is a proposed draft (live in recall; a human confirm binds it);
// an updated/skipped record means the decision already existed (an import never
// changes its lifecycle status), so the tool must NOT re-announce a fresh draft.
func TestRecordToHosted_StatusFollowsServerResult(t *testing.T) {
	dr := sampleDecisionRecord()
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	mk := func(status string) func(context.Context, []pushRecord) (pushResponse, error) {
		return func(context.Context, []pushRecord) (pushResponse, error) {
			return pushResponse{Results: []pushResult{{Status: status}}}, nil
		}
	}
	cases := []struct {
		serverStatus    string
		wantStatus      string
		wantContains    string
		wantNotContains string
	}{
		{"created", "proposed", "confirm", "inbox"},
		{"updated", "updated", "updated", "inbox"},
		{"skipped", "skipped", "already recorded", "inbox"},
	}
	for _, tc := range cases {
		out, err := recordToHosted(context.Background(), dr, now, mk(tc.serverStatus))
		if err != nil {
			t.Fatalf("server %q: unexpected error: %v", tc.serverStatus, err)
		}
		if out.Status != tc.wantStatus {
			t.Errorf("server %q: out.Status=%q want %q", tc.serverStatus, out.Status, tc.wantStatus)
		}
		if !strings.Contains(strings.ToLower(out.Recorded), tc.wantContains) {
			t.Errorf("server %q: msg %q must contain %q", tc.serverStatus, out.Recorded, tc.wantContains)
		}
		if tc.wantNotContains != "" && strings.Contains(out.Recorded, tc.wantNotContains) {
			t.Errorf("server %q: msg %q must NOT tell user to accept a decision that already exists", tc.serverStatus, out.Recorded)
		}
	}
}

// #1: a non-fatal server warning (e.g. an unresolvable supersedes target) rides
// back on a Failed=0 result and MUST be surfaced, never swallowed.
func TestRecordToHosted_SurfacesWarnings(t *testing.T) {
	push := func(context.Context, []pushRecord) (pushResponse, error) {
		return pushResponse{
			Created: 1,
			Results: []pushResult{{Status: "created", Warnings: []string{`unresolvable supersedes target "d_old"`}}},
		}, nil
	}
	out, err := recordToHosted(context.Background(), sampleDecisionRecord(), time.Now(), push)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out.Recorded, "unresolvable supersedes target") {
		t.Errorf("a non-fatal server warning must be surfaced, not swallowed: %q", out.Recorded)
	}
}

// Fail loud: a hosted push failure returns an error, never a silent success and
// never a silent fall-back to the local store.
func TestRecordToHosted_PushFailureIsError(t *testing.T) {
	push := func(context.Context, []pushRecord) (pushResponse, error) {
		return pushResponse{}, errors.New("connection refused")
	}
	if _, err := recordToHosted(context.Background(), sampleDecisionRecord(), time.Now(), push); err == nil {
		t.Fatal("want an error when the hosted push fails, got nil")
	}
}

// A server-reported per-record failure is also an error.
func TestRecordToHosted_ServerFailedRecordIsError(t *testing.T) {
	push := func(context.Context, []pushRecord) (pushResponse, error) {
		return pushResponse{
			Failed:  1,
			Results: []pushResult{{Status: "failed", Reason: "unknown workspace"}},
		}, nil
	}
	if _, err := recordToHosted(context.Background(), sampleDecisionRecord(), time.Now(), push); err == nil {
		t.Fatal("want an error when the server fails the record, got nil")
	}
}

// #5: title and chosen are required, validated before either sink runs, so an
// empty field fails the same clean way in hosted and solo mode.
func TestRecorder_RequiresTitleAndChosen(t *testing.T) {
	fake := &fakeCapture{}
	r := recorder{capture: fake}
	for _, dr := range []source.DecisionRecord{
		{Title: "", Chosen: "c"},
		{Title: "  ", Chosen: "c"},
		{Title: "t", Chosen: ""},
	} {
		if _, err := r.record(context.Background(), dr); err == nil {
			t.Errorf("want error for %+v", dr)
		}
	}
	if fake.called {
		t.Fatal("validation must happen before the sink is called")
	}
}

// The recorder routes to the hosted sink when one is set (hosted mode).
func TestRecorder_RoutesToHostedSink(t *testing.T) {
	called := false
	r := recorder{pushHosted: func(_ context.Context, dr source.DecisionRecord) (recordOutput, error) {
		called = true
		return recordOutput{ID: "h1", Status: "proposed", Recorded: "drafted; accept in-app"}, nil
	}}
	out, err := r.record(context.Background(), source.DecisionRecord{Title: "t", Chosen: "c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("hosted sink must be used when pushHosted is set")
	}
	if out.Status != "proposed" {
		t.Errorf("status = %q, want proposed", out.Status)
	}
}

// The recorder routes to the local capture store in solo mode, mapping all fields
// and surfacing the immediate-bind message.
func TestRecorder_RoutesToLocalCaptureInSoloMode(t *testing.T) {
	fake := &fakeCapture{}
	r := recorder{capture: fake}
	dr := sampleDecisionRecord()

	out, err := r.record(context.Background(), dr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fake.called {
		t.Fatal("local capture must be used in solo mode")
	}
	if fake.got.Title != dr.Title || fake.got.Chosen != dr.Chosen {
		t.Errorf("captured record not mapped: %+v", fake.got)
	}
	if !reflect.DeepEqual(fake.got.Rejected, dr.Rejected) {
		t.Errorf("rejected not mapped to the capture record: %+v", fake.got.Rejected)
	}
	if out.ID != "d_fake01" || out.Status != "accepted" {
		t.Errorf("output = %+v, want the stored id and accepted status", out)
	}
	if !strings.Contains(out.Recorded, "CLOSED") {
		t.Errorf("solo message %q should report the rejected alternatives now CLOSED", out.Recorded)
	}
}

// A zero recorder (no sink wired) records nothing and fails loud.
func TestRecorder_NoSinkErrors(t *testing.T) {
	r := recorder{}
	if _, err := r.record(context.Background(), source.DecisionRecord{Title: "t", Chosen: "c"}); err == nil {
		t.Fatal("want an error when no sink is configured, got nil")
	}
}

// #6: the MCP input maps onto the canonical capture model in one place.
func TestRecordInput_ToDecisionRecord(t *testing.T) {
	in := recordInput{
		Title: "t", Chosen: "c",
		Rejected:   []source.RejectedAlt{{Option: "x", Why: "y"}},
		Constraint: "k", Consequence: "z", Supersedes: []string{"d_1"},
		Refs: []string{"f.go"}, Rationale: "r",
	}
	dr := in.toDecisionRecord()
	if dr.Title != "t" || dr.Chosen != "c" || dr.Constraint != "k" || dr.Consequence != "z" || dr.Rationale != "r" {
		t.Errorf("scalar fields not mapped: %+v", dr)
	}
	if !reflect.DeepEqual(dr.Rejected, in.Rejected) || !reflect.DeepEqual(dr.Supersedes, in.Supersedes) || !reflect.DeepEqual(dr.Refs, in.Refs) {
		t.Errorf("slice fields not mapped: %+v", dr)
	}
}

// #3: the GUI /api/record handler routes through decisionRecorder (so hosted mode
// drafts a proposed capture), not capture.Record directly (which would bind it
// locally-accepted — the ADR-0125 poison).
func TestHttpRecord_RoutesThroughRecorder(t *testing.T) {
	prevRec, prevCap := decisionRecorder, capture
	t.Cleanup(func() { decisionRecorder, capture = prevRec, prevCap })

	called := false
	capture = nil // httpRecord must not touch the local store directly
	decisionRecorder = recorder{pushHosted: func(_ context.Context, dr source.DecisionRecord) (recordOutput, error) {
		called = true
		return recordOutput{ID: "dec_1", Status: "proposed", Recorded: "drafted; accept in inbox"}, nil
	}}

	req := httptest.NewRequest(http.MethodPost, "http://x/api/record", strings.NewReader(`{"title":"t","chosen":"c"}`))
	w := httptest.NewRecorder()
	httpRecord(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if !called {
		t.Fatal("httpRecord must route through decisionRecorder (hosted proposed), not capture.Record (local accepted)")
	}
	if !strings.Contains(w.Body.String(), "proposed") {
		t.Errorf("response should carry the recorder output: %s", w.Body.String())
	}
}
