package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The keystone: a check_approach that found no recorded ruling, then an edit that
// adopts the checked approach, yields exactly one proposed-decision candidate
// carrying the approach and the edited file. This is the loop's producer firing.
func TestDetectCandidates_NoRulingThenAdoptingEdit(t *testing.T) {
	events := []transcriptEvent{
		{kind: evCheckCall, toolUseID: "t1", approach: "Redis for the session store"},
		{kind: evCheckResult, toolUseID: "t1", verdict: "no_recorded_ruling"},
		{kind: evEdit,
			editText: "store.go cache, err := redis.NewClient(opt) // session store in redis",
			refs:     []string{"internal/session/store.go"}},
	}

	got := detectCandidates(events)

	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(got), got)
	}
	if got[0].Approach != "Redis for the session store" {
		t.Errorf("approach = %q, want %q", got[0].Approach, "Redis for the session store")
	}
	if !reflect.DeepEqual(got[0].Refs, []string{"internal/session/store.go"}) {
		t.Errorf("refs = %v, want [internal/session/store.go]", got[0].Refs)
	}
}

// Determinism (design §6): the same transcript must always produce the same
// drafts in the same order — no model, no map-iteration nondeterminism. Two
// no-ruling approaches adopted by one edit must surface in the order they were
// checked, identically across runs.
func TestDetectCandidates_DeterministicOrder(t *testing.T) {
	events := []transcriptEvent{
		{kind: evCheckCall, toolUseID: "a", approach: "Redis"},
		{kind: evCheckResult, toolUseID: "a", verdict: "no_recorded_ruling"},
		{kind: evCheckCall, toolUseID: "b", approach: "Postgres"},
		{kind: evCheckResult, toolUseID: "b", verdict: "no_recorded_ruling"},
		{kind: evEdit,
			editText: "db.go redis client and postgres pool wired here",
			refs:     []string{"db.go"}},
	}

	want := []string{"Redis", "Postgres"} // checked order
	for i := range 20 {
		got := detectCandidates(events)
		var order []string
		for _, c := range got {
			order = append(order, c.Approach)
		}
		if !reflect.DeepEqual(order, want) {
			t.Fatalf("run %d: order = %v, want %v (must be stable, check-order)", i, order, want)
		}
	}
}

// The negatives — each guards a distinct reason Signal A must NOT draft, so a
// future change that loosens precision (drafting where the agent should comply,
// or on an unrelated edit) fails loudly.
func TestDetectCandidates_NoDraftCases(t *testing.T) {
	cases := []struct {
		name   string
		events []transcriptEvent
		why    string
	}{
		{
			name: "ruled_out suppresses (agent should comply, not draft)",
			events: []transcriptEvent{
				{kind: evCheckCall, toolUseID: "t1", approach: "Redis"},
				{kind: evCheckResult, toolUseID: "t1", verdict: "ruled_out"},
				{kind: evEdit, editText: "cache.go redis.NewClient()", refs: []string{"cache.go"}},
			},
			why: "a recorded ruling means the answer exists; drafting it would re-record settled ground",
		},
		{
			name: "settled suppresses",
			events: []transcriptEvent{
				{kind: evCheckCall, toolUseID: "t1", approach: "Redis"},
				{kind: evCheckResult, toolUseID: "t1", verdict: "settled"},
				{kind: evEdit, editText: "cache.go redis.NewClient()", refs: []string{"cache.go"}},
			},
			why: "an in-force choice is already in the corpus",
		},
		{
			name: "edit shares no scope (precision floor)",
			events: []transcriptEvent{
				{kind: evCheckCall, toolUseID: "t1", approach: "Redis for caching"},
				{kind: evCheckResult, toolUseID: "t1", verdict: "no_recorded_ruling"},
				{kind: evEdit, editText: "README.md fixed a typo in the intro", refs: []string{"README.md"}},
			},
			why: "an edit unrelated to the checked approach is not an adoption of it",
		},
		{
			name: "no adopting edit at all",
			events: []transcriptEvent{
				{kind: evCheckCall, toolUseID: "t1", approach: "Redis"},
				{kind: evCheckResult, toolUseID: "t1", verdict: "no_recorded_ruling"},
			},
			why: "checking without then adopting is not a decision",
		},
		{
			name: "edit precedes the no-ruling result (wrong order)",
			events: []transcriptEvent{
				{kind: evCheckCall, toolUseID: "t1", approach: "Redis"},
				{kind: evEdit, editText: "cache.go redis.NewClient()", refs: []string{"cache.go"}},
				{kind: evCheckResult, toolUseID: "t1", verdict: "no_recorded_ruling"},
			},
			why: "the edit was not made in response to the no-ruling finding",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectCandidates(tc.events); len(got) != 0 {
				t.Errorf("want 0 candidates (%s), got %d: %+v", tc.why, len(got), got)
			}
		})
	}
}

// The parser distills a real Claude Code transcript (the exact on-disk shape:
// MCP tool_use named mcp__lema__check_approach, a user tool_result whose content
// is a JSON string carrying "verdict", and an Edit tool_use) into ordered events.
func TestParseTranscriptEvents_RealShape(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"mcp__lema__check_approach","input":{"repo":"rust","approach":"Redis for caching"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"{\"approach\":\"Redis for caching\",\"verdict\":\"no_recorded_ruling\",\"sources\":[]}"}]},"toolUseResult":"{\"verdict\":\"no_recorded_ruling\"}"}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Edit","input":{"file_path":"internal/cache/store.go","old_string":"x","new_string":"added redis client for the caching layer"}}]}}`,
	}

	evts, err := parseTranscriptEvents(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(evts) != 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(evts), evts)
	}
	if evts[0].kind != evCheckCall || evts[0].toolUseID != "toolu_1" || evts[0].approach != "Redis for caching" {
		t.Errorf("event0 (check call) = %+v", evts[0])
	}
	if evts[1].kind != evCheckResult || evts[1].toolUseID != "toolu_1" || evts[1].verdict != "no_recorded_ruling" {
		t.Errorf("event1 (check result) = %+v", evts[1])
	}
	if evts[2].kind != evEdit {
		t.Errorf("event2 should be an edit, got kind %v", evts[2].kind)
	}
	if !reflect.DeepEqual(evts[2].refs, []string{"internal/cache/store.go"}) {
		t.Errorf("event2 refs = %v, want [internal/cache/store.go]", evts[2].refs)
	}
	if !strings.Contains(evts[2].editText, "redis") {
		t.Errorf("event2 editText should carry the edit's new text, got %q", evts[2].editText)
	}
}

// A doc/markdown edit must NOT count as an adoption: writing prose ABOUT an
// approach (an ADR, notes, a docs scenario) is not adopting it in code, and
// real-data scanning showed doc edits are the dominant false-positive source.
func TestParseTranscriptEvents_SkipsDocFileEdits(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"docs/notes.md","old_string":"x","new_string":"discussion of redis caching and async traits"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"e2","name":"Write","input":{"file_path":"plan.txt","content":"we will use redis for caching"}}]}}`,
	}
	evts, _ := parseTranscriptEvents(strings.NewReader(strings.Join(lines, "\n")))
	for _, e := range evts {
		if e.kind == evEdit {
			t.Errorf("a doc-text edit must not be an adoption edit; got %+v", e)
		}
	}
}

// End to end over the real shape: parse + detect yields one proposed candidate.
func TestScanTranscriptForCandidates_EndToEnd(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_1","name":"mcp__lema__check_approach","input":{"repo":"rust","approach":"Redis for caching"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"{\"verdict\":\"no_recorded_ruling\"}"}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"toolu_2","name":"Write","input":{"file_path":"cache.go","content":"redis client for caching"}}]}}`,
	}
	dir := t.TempDir()
	path := dir + "/session.jsonl"
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := scanTranscriptForCandidates(path)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d: %+v", len(got), got)
	}
	if got[0].Approach != "Redis for caching" {
		t.Errorf("approach = %q", got[0].Approach)
	}
}

// The producer gate is now the hosted env-wide WorkOS flag lema-fuse-push
// (ADR-0111), fetched from the API rather than read off a local env var: the
// Stop hook has no WorkOS session and must not hold WORKOS_API_KEY. The client
// asks GET /push-enabled (Bearer auth) and fails CLOSED — it scans/transmits
// only when the API affirmatively returns enabled:true.
func TestPushProducerEnabled(t *testing.T) {
	t.Run("enabled true -> on; sends GET /push-enabled with bearer auth", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": true})
		}))
		defer srv.Close()
		if !pushProducerEnabled(context.Background(), srv.Client(), srv.URL, "lema_live_abc") {
			t.Error("want enabled (true)")
		}
		if gotMethod != http.MethodGet || gotPath != "/push-enabled" {
			t.Errorf("request = %s %s, want GET /push-enabled", gotMethod, gotPath)
		}
		if gotAuth != "Bearer lema_live_abc" {
			t.Errorf("auth = %q, want Bearer lema_live_abc", gotAuth)
		}
	})

	t.Run("enabled false -> off", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": false})
		}))
		defer srv.Close()
		if pushProducerEnabled(context.Background(), srv.Client(), srv.URL, "t") {
			t.Error("want disabled (false)")
		}
	})

	t.Run("server error -> fail closed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		if pushProducerEnabled(context.Background(), srv.Client(), srv.URL, "t") {
			t.Error("5xx must fail closed (false)")
		}
	})

	t.Run("unreachable API -> fail closed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		if pushProducerEnabled(context.Background(), &http.Client{}, url, "t") {
			t.Error("transport error must fail closed (false)")
		}
	})
}

// The orchestration is fail-open and silent: it pushes only when credentials
// resolved, a transcript exists, this isn't a re-entrant stop, and candidates
// were found — and any scan/push failure degrades to a no-op (never wedges the
// session, never blocks the stop).
func TestPushRunner(t *testing.T) {
	cands := []pushCandidate{{Approach: "Redis", Refs: []string{"c.go"}}}
	base := stopHookInput{TranscriptPath: "/x/session.jsonl"}
	okPush := func(context.Context, []pushRecord) (pushResponse, error) { return pushResponse{Created: 1}, nil }
	gateOn := func(context.Context) bool { return true }

	t.Run("producer disabled (gate false) -> no scan, no push", func(t *testing.T) {
		scanned, pushed := false, false
		r := pushRunner{
			gate: func(context.Context) bool { return false },
			scan: func(string) ([]pushCandidate, error) { scanned = true; return cands, nil },
			push: func(context.Context, []pushRecord) (pushResponse, error) { pushed = true; return pushResponse{}, nil },
			now:  time.Now, canPush: true,
		}
		if n := r.run(context.Background(), base); n != 0 || scanned || pushed {
			t.Errorf("n=%d scanned=%v pushed=%v, want 0/false/false — a disabled producer must not read the transcript or transmit", n, scanned, pushed)
		}
	})

	t.Run("no gate wired -> fail-closed no-op", func(t *testing.T) {
		scanned := false
		r := pushRunner{
			scan: func(string) ([]pushCandidate, error) { scanned = true; return cands, nil },
			push: okPush, now: time.Now, canPush: true, // gate nil
		}
		if n := r.run(context.Background(), base); n != 0 || scanned {
			t.Errorf("n=%d scanned=%v, want 0/false — a nil gate must fail closed", n, scanned)
		}
	})

	t.Run("no credentials -> no scan, no push", func(t *testing.T) {
		scanned, pushed := false, false
		r := pushRunner{
			scan: func(string) ([]pushCandidate, error) { scanned = true; return cands, nil },
			push: func(context.Context, []pushRecord) (pushResponse, error) { pushed = true; return pushResponse{}, nil },
			now:  time.Now, canPush: false,
		}
		if n := r.run(context.Background(), base); n != 0 || scanned || pushed {
			t.Errorf("n=%d scanned=%v pushed=%v, want 0/false/false", n, scanned, pushed)
		}
	})

	t.Run("stop_hook_active -> no-op (loop guard)", func(t *testing.T) {
		scanned := false
		in := base
		in.StopHookActive = true
		r := pushRunner{scan: func(string) ([]pushCandidate, error) { scanned = true; return cands, nil }, push: okPush, now: time.Now, canPush: true}
		if n := r.run(context.Background(), in); n != 0 || scanned {
			t.Errorf("n=%d scanned=%v, want 0/false", n, scanned)
		}
	})

	t.Run("empty transcript path -> no-op", func(t *testing.T) {
		r := pushRunner{scan: func(string) ([]pushCandidate, error) { return cands, nil }, push: okPush, now: time.Now, canPush: true}
		if n := r.run(context.Background(), stopHookInput{}); n != 0 {
			t.Errorf("n=%d, want 0", n)
		}
	})

	t.Run("happy path -> drafts proposed", func(t *testing.T) {
		var got []pushRecord
		r := pushRunner{
			gate: gateOn,
			scan: func(string) ([]pushCandidate, error) { return cands, nil },
			push: func(_ context.Context, recs []pushRecord) (pushResponse, error) {
				got = recs
				return pushResponse{Created: len(recs)}, nil
			},
			now: func() time.Time { return time.Unix(0, 0).UTC() }, canPush: true,
		}
		if n := r.run(context.Background(), base); n != 1 {
			t.Fatalf("n=%d, want 1", n)
		}
		if len(got) != 1 || got[0].Status != "proposed" {
			t.Errorf("pushed=%+v, want one proposed record", got)
		}
	})

	t.Run("scan error -> fail-open no-op", func(t *testing.T) {
		r := pushRunner{gate: gateOn, scan: func(string) ([]pushCandidate, error) { return nil, errors.New("boom") }, push: okPush, now: time.Now, canPush: true}
		if n := r.run(context.Background(), base); n != 0 {
			t.Errorf("n=%d, want 0 on scan error", n)
		}
	})

	t.Run("push error -> fail-open no-op", func(t *testing.T) {
		r := pushRunner{
			gate: gateOn,
			scan: func(string) ([]pushCandidate, error) { return cands, nil },
			push: func(context.Context, []pushRecord) (pushResponse, error) { return pushResponse{}, errors.New("boom") },
			now:  time.Now, canPush: true,
		}
		if n := r.run(context.Background(), base); n != 0 {
			t.Errorf("n=%d, want 0 on push error", n)
		}
	})
}

// The push client speaks the server's contract exactly: POST to the workspace
// import path, Bearer auth, schema_version 1, status proposed, and it reads back
// the server-derived recorded_by so it can render "recorded as agent" honestly.
func TestPushDecisions_RequestShapeAndAuth(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotReq pushRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(pushResponse{Created: 1, RecordedBy: "agent",
			Results: []pushResult{{LocalID: "d_x", Status: "created"}}})
	}))
	defer srv.Close()

	recs := []pushRecord{{ID: "d_x", Title: "Redis", Chosen: "Redis", Status: "proposed", Refs: []string{"cache.go"}}}
	resp, err := pushDecisions(context.Background(), srv.Client(), srv.URL, "lema_live_abc", "ws_123", recs)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/workspaces/ws_123/import-decisions" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer lema_live_abc" {
		t.Errorf("auth = %q, want Bearer lema_live_abc", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotReq.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1 (server rejects others)", gotReq.SchemaVersion)
	}
	if len(gotReq.Records) != 1 || gotReq.Records[0].Status != "proposed" {
		t.Errorf("records = %+v, want one proposed record", gotReq.Records)
	}
	if resp.RecordedBy != "agent" {
		t.Errorf("recordedBy = %q, want agent (client must render the honest provenance)", resp.RecordedBy)
	}
}

// A non-2xx from the server is an error, never a silent success — the hook decides
// to swallow it (fail-open), but the client must surface it.
func TestPushDecisions_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unsupported schema_version (want 1)", http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := pushDecisions(context.Background(), srv.Client(), srv.URL, "t", "ws", []pushRecord{{ID: "d", Status: "proposed"}})
	if err == nil {
		t.Fatal("want an error on HTTP 400, got nil")
	}
}

// The candidate→record mapping drafts as proposed with NO rejected alternative
// (the deterministic signal can't see the counterfactual) and a stable
// content-keyed id — so a draft can only add noise, never self-bind or poison.
func TestCandidateRecords_ProposedNoRejectedStableID(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	recs := candidateRecords([]pushCandidate{{Approach: "Redis for caching", Refs: []string{"cache.go"}}}, now)
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	r := recs[0]
	if r.Status != "proposed" {
		t.Errorf("status = %q, want proposed (a draft cannot self-bind)", r.Status)
	}
	if len(r.Rejected) != 0 {
		t.Errorf("Signal A is deterministic — no rejected alternative; got %+v", r.Rejected)
	}
	if r.Chosen != "Redis for caching" {
		t.Errorf("chosen = %q", r.Chosen)
	}
	if r.TS != "2026-06-23T12:00:00Z" {
		t.Errorf("ts = %q, want the stamped time", r.TS)
	}
	if r.ID == "" {
		t.Error("want a content-keyed id for dedup")
	}
	if !reflect.DeepEqual(r.Refs, []string{"cache.go"}) {
		t.Errorf("refs = %v", r.Refs)
	}
}

// Two distinct approaches, each adopted by its own edit, yield two candidates —
// the loop producing more than one draft per session.
func TestDetectCandidates_MultipleDistinctApproaches(t *testing.T) {
	events := []transcriptEvent{
		{kind: evCheckCall, toolUseID: "t1", approach: "Redis"},
		{kind: evCheckResult, toolUseID: "t1", verdict: "no_recorded_ruling"},
		{kind: evEdit, editText: "cache.go redis.NewClient()", refs: []string{"cache.go"}},
		{kind: evCheckCall, toolUseID: "t2", approach: "server components"},
		{kind: evCheckResult, toolUseID: "t2", verdict: "no_recorded_ruling"},
		{kind: evEdit, editText: "page.tsx default export, server components only", refs: []string{"page.tsx"}},
	}
	got := detectCandidates(events)
	if len(got) != 2 {
		t.Fatalf("want 2 candidates, got %d: %+v", len(got), got)
	}
}
