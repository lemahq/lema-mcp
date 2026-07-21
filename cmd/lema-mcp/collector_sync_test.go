package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// The sync half's contract: boundaries sync the checkpoint THIS run produced
// to the hosted run journal (create-run idempotent, then a 'checkpoint'
// event); missing config or any server failure — including the 404 a dark
// lema-run-state serves — is silent local-only. The spool stays the truth.

type syncCapture struct {
	runCreates int
	events     []map[string]any
}

func newSyncTestServer(t *testing.T, cap *syncCapture, eventStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workspaces/ws-1/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token")
		}
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["harness"] != "claude-code" || req["external_run_id"] == "" {
			t.Errorf("run create body = %#v", req)
		}
		// First create → 201; adapter retry → 200 with the same identity
		// (the server contract createRun documents).
		status := http.StatusCreated
		if cap.runCreates > 0 {
			status = http.StatusOK
		}
		cap.runCreates++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"run":{"id":"11111111-1111-1111-1111-111111111111"},"created":true,"rung":7}`))
	})
	mux.HandleFunc("POST /workspaces/ws-1/runs/11111111-1111-1111-1111-111111111111/events", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		cap.events = append(cap.events, req)
		w.WriteHeader(eventStatus)
		_, _ = w.Write([]byte(`{"id":"e","kind":"checkpoint","payload":{},"created_at":"2026-07-21T00:00:00Z"}`))
	})
	return httptest.NewServer(mux)
}

func setSyncEnv(t *testing.T, url string) {
	t.Helper()
	t.Setenv("LEMA_API_URL", url)
	t.Setenv("LEMA_API_TOKEN", "tok")
	t.Setenv("LEMA_WORKSPACE_ID", "ws-1")
}

func writeTestCheckpoint(t *testing.T, dir, runID string) collectorCheckpoint {
	t.Helper()
	cp := distillEnvelopes([]collectorEnvelope{
		mkEnv(runID, "user_prompt", map[string]string{"prompt": "sync me"}),
		mkEnv(runID, "tool_use", map[string]string{"file_path": "a.go"}),
	}, "/repo/proj")
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	return cp
}

func TestSyncCheckpointPostsRunThenEvent(t *testing.T) {
	cap := &syncCapture{}
	srv := newSyncTestServer(t, cap, http.StatusCreated)
	defer srv.Close()
	setSyncEnv(t, srv.URL)

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-sync")
	syncOnBoundary(dir, "claude-code", mkEnv("sess-sync", "stop", nil))
	// A second boundary re-syncs (server subsumption makes it safe) and
	// exercises the 200-on-retry create branch.
	syncOnBoundary(dir, "claude-code", mkEnv("sess-sync", "session_end", nil))

	if len(cap.events) != 2 {
		t.Fatalf("want two checkpoint events across two boundaries, got %d", len(cap.events))
	}
	if cap.runCreates != 2 {
		t.Fatalf("run create must be called per sync (idempotent server-side), got %d", cap.runCreates)
	}
	ev := cap.events[0]
	if ev["kind"] != "checkpoint" {
		t.Fatalf("kind = %v", ev["kind"])
	}
	payload, _ := ev["payload"].(map[string]any)
	if payload["summary"] == "" || payload["cwd"] != "/repo/proj" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestSyncRefusesFileSourcedWorkspace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no HTTP call may happen when the workspace comes from the credentials file, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	home := t.TempDir()
	t.Setenv("HOME", home)
	credDir := home + "/.config/lema"
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credDir+"/credentials",
		[]byte("LEMA_API_URL="+srv.URL+"\nLEMA_API_TOKEN=tok\nLEMA_WORKSPACE_ID=vestige-ws\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Env supplies nothing — everything would have to come from the file.
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")
	t.Setenv("LEMA_WORKSPACE_ID", "")

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-v")
	out := captureStdout(t, func() {
		syncOnBoundary(dir, "claude-code", mkEnv("sess-v", "stop", nil))
	})
	if out != "" {
		t.Fatalf("sync paths must write nothing to stdout, got %q", out)
	}
}

func TestSyncSkipsNonBoundaryAndForeignRun(t *testing.T) {
	cap := &syncCapture{}
	srv := newSyncTestServer(t, cap, http.StatusCreated)
	defer srv.Close()
	setSyncEnv(t, srv.URL)

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-owner")
	// Non-boundary kinds never sync.
	syncOnBoundary(dir, "claude-code", mkEnv("sess-owner", "tool_use", nil))
	// A DIFFERENT run's boundary must not re-send this run's checkpoint.
	syncOnBoundary(dir, "claude-code", mkEnv("sess-other", "stop", nil))
	if len(cap.events) != 0 {
		t.Fatalf("nothing should sync, got %d events", len(cap.events))
	}
}

func TestSyncFailOpenPaths(t *testing.T) {
	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-x")

	// No config at all: newCollectorSyncer must bail before any HTTP.
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")
	t.Setenv("LEMA_WORKSPACE_ID", "")
	t.Setenv("HOME", t.TempDir()) // isolate from any real ~/.config/lema/credentials
	syncOnBoundary(dir, "claude-code", mkEnv("sess-x", "stop", nil))

	// Dark server (404 while lema-run-state is off): silent.
	cap := &syncCapture{}
	dark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer dark.Close()
	setSyncEnv(t, dark.URL)
	syncOnBoundary(dir, "claude-code", mkEnv("sess-x", "stop", nil))

	// Unreachable server: silent.
	setSyncEnv(t, "http://127.0.0.1:1")
	syncOnBoundary(dir, "claude-code", mkEnv("sess-x", "stop", nil))
	// Reaching here without panics or hangs IS the assertion; cap unused.
	_ = cap
}

func TestSyncCheckpointEventRejectedIsAnError(t *testing.T) {
	cap := &syncCapture{}
	srv := newSyncTestServer(t, cap, http.StatusBadRequest)
	defer srv.Close()
	setSyncEnv(t, srv.URL)

	s := newCollectorSyncer()
	if s == nil {
		t.Fatal("syncer must resolve from env")
	}
	cp := distillEnvelopes([]collectorEnvelope{
		mkEnv("sess-r", "user_prompt", map[string]string{"prompt": "x"}),
	}, "/repo/proj")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.syncCheckpoint(ctx, "claude-code", cp); err == nil {
		t.Fatal("a rejected event must surface as an error to the (ignoring) caller")
	}
}
