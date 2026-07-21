package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// get_state_brief's contract: explicit run wins; otherwise the project's
// prior run resolves from the local F4 checkpoint using the SAME harness key
// the collector synced with (a different key would mint a second hosted
// identity); a dark server (404) and missing config are honest notes, never
// errors or fabricated scope.

func newBriefTestServer(t *testing.T, wantHarness string, briefStatus int) (*httptest.Server, *syncCapture) {
	t.Helper()
	cap := &syncCapture{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workspaces/ws-1/runs", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["harness"] != wantHarness {
			t.Errorf("harness = %q, want %q (a drifted key mints a second identity)", req["harness"], wantHarness)
		}
		cap.runCreates++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"run":{"id":"22222222-2222-2222-2222-222222222222"},"created":false,"rung":7}`))
	})
	mux.HandleFunc("GET /workspaces/ws-1/brief", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token")
		}
		cap.events = append(cap.events, map[string]any{"run": r.URL.Query().Get("run")})
		w.WriteHeader(briefStatus)
		_, _ = w.Write([]byte(`{"scope":"work unit wu-1","sections":[{"name":"objective","lines":[{"text":"ship it","cite":"work_unit:wu-1"}]}],"silences":["test status — not captured in v1"],"as_of":"2026-07-21T00:00:00Z"}`))
	})
	return httptest.NewServer(mux), cap
}

func TestGetStateBriefExplicitRun(t *testing.T) {
	srv, cap := newBriefTestServer(t, "", http.StatusOK)
	defer srv.Close()
	setSyncEnv(t, srv.URL)

	_, out, err := getStateBrief(context.Background(), nil, stateBriefInput{Run: "22222222-2222-2222-2222-222222222222"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Scope != "work unit wu-1" || !strings.Contains(out.Note, "explicit") {
		t.Fatalf("out = %+v", out)
	}
	if cap.runCreates != 0 {
		t.Fatal("an explicit run must not touch run creation")
	}
	var sections []map[string]any
	if err := json.Unmarshal(out.Sections, &sections); err != nil || len(sections) != 1 {
		t.Fatalf("sections must pass through verbatim: %s", out.Sections)
	}
	if len(out.Silences) == 0 {
		t.Fatal("silences must pass through — they are the honesty half")
	}
}

func TestGetStateBriefResolvesPriorRunFromCheckpoint(t *testing.T) {
	srv, cap := newBriefTestServer(t, "claude-code", http.StatusOK)
	defer srv.Close()
	setSyncEnv(t, srv.URL)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv(collectorDirEnv, dir)
	cp := distillEnvelopes([]collectorEnvelope{{
		RunID: "sess-prior", TS: time.Now().UTC().Format(time.RFC3339), Kind: "user_prompt",
		Payload:  map[string]string{"prompt": "resume me"},
		Evidence: map[string]string{"harness": "claude-code", "cwd": cwd},
	}}, cwd)
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}

	_, out, err := getStateBrief(context.Background(), nil, stateBriefInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Note, "sess-prior") {
		t.Fatalf("note must attribute the resolved prior run: %+v", out)
	}
	if cap.runCreates != 1 {
		t.Fatalf("prior-run resolution must ensure the hosted identity once, got %d", cap.runCreates)
	}
	if len(cap.events) != 1 || cap.events[0]["run"] != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("brief must be fetched for the ensured hosted run: %v", cap.events)
	}
}

func TestGetStateBriefHonestWhenUnavailable(t *testing.T) {
	// No checkpoint for this project → honest note, no fabricated scope.
	srv, _ := newBriefTestServer(t, "claude-code", http.StatusOK)
	defer srv.Close()
	setSyncEnv(t, srv.URL)
	t.Setenv(collectorDirEnv, t.TempDir())
	_, out, err := getStateBrief(context.Background(), nil, stateBriefInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Scope != "" || !strings.Contains(out.Note, "no prior run known") {
		t.Fatalf("out = %+v", out)
	}

	// Dark surface (404 while lema-state-brief is off) → honest note.
	dark, _ := newBriefTestServer(t, "", http.StatusNotFound)
	defer dark.Close()
	setSyncEnv(t, dark.URL)
	_, out, err = getStateBrief(context.Background(), nil, stateBriefInput{Run: "22222222-2222-2222-2222-222222222222"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Scope != "" || !strings.Contains(out.Note, "unavailable") {
		t.Fatalf("dark surface must be an honest note: %+v", out)
	}

	// No hosted config at all.
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")
	t.Setenv("LEMA_WORKSPACE_ID", "")
	t.Setenv("HOME", t.TempDir())
	_, out, err = getStateBrief(context.Background(), nil, stateBriefInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Note, "not configured") {
		t.Fatalf("missing config must be named: %+v", out)
	}
}
