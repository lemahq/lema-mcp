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
	runCreate  map[string]string // the last run-create body (harness/external_run_id/repo/branch/worktree)
	events     []map[string]any
}

func newSyncTestServer(t *testing.T, cap *syncCapture, eventStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workspaces", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token on workspace validation")
		}
		_, _ = w.Write([]byte(`{"workspaces":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}]}`))
	})
	mux.HandleFunc("POST /workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token")
		}
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["harness"] != "claude-code" || req["external_run_id"] == "" {
			t.Errorf("run create body = %#v", req)
		}
		cap.runCreate = req
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
	mux.HandleFunc("POST /workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/runs/11111111-1111-1111-1111-111111111111/events", func(w http.ResponseWriter, r *http.Request) {
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
	t.Setenv("LEMA_WORKSPACE_ID", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
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

// TestSyncSendsRepoAndBranchOnRunCreate pins repo-on-run-create (decision
// 5025ffb7): run-create now carries repo (lowercased owner/name) + branch +
// worktree(=cwd), so the server ladder can reach rungs 3/4 instead of always
// landing rung 7.
func TestSyncSendsRepoAndBranchOnRunCreate(t *testing.T) {
	restoreRemote, restoreBranch := gitRemoteURL, gitCurrentBranch
	t.Cleanup(func() { gitRemoteURL, gitCurrentBranch = restoreRemote, restoreBranch })
	gitRemoteURL = func(string) (string, bool) { return "git@github.com:LemaHQ/Lema.git", true }
	gitCurrentBranch = func(string) (string, bool) { return "feat/x", true }

	cap := &syncCapture{}
	srv := newSyncTestServer(t, cap, http.StatusCreated)
	defer srv.Close()
	setSyncEnv(t, srv.URL)

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-repo") // checkpoint cwd = "/repo/proj"
	syncOnBoundary(dir, "claude-code", mkEnv("sess-repo", "stop", nil))

	rc := cap.runCreate
	if rc["repo"] != "lemahq/lema" {
		t.Errorf("repo = %q, want lowercased owner/name 'lemahq/lema' (rung 3 needs it)", rc["repo"])
	}
	if rc["branch"] != "feat/x" {
		t.Errorf("branch = %q, want feat/x (rung 3)", rc["branch"])
	}
	if rc["worktree"] != "/repo/proj" {
		t.Errorf("worktree = %q, want the run cwd (rung 4)", rc["worktree"])
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
	// Env supplies URL/token from the file but no workspace pin. The workspace
	// is then DERIVED from the git remote — never read from the per-user file.
	// Stub the git read to find nothing, so the file's vestige-ws is provably
	// never the target: no pin, no derivable remote → nothing syncs.
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")
	t.Setenv("LEMA_WORKSPACE_ID", "")
	restoreGit := gitRemoteURL
	t.Cleanup(func() { gitRemoteURL = restoreGit })
	gitRemoteURL = func(string) (string, bool) { return "", false }

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-v")
	out := captureStdout(t, func() {
		syncOnBoundary(dir, "claude-code", mkEnv("sess-v", "stop", nil))
	})
	if out != "" {
		t.Fatalf("sync paths must write nothing to stdout, got %q", out)
	}
}

// Zero-config multi-repo (decision d_d9caf0): with no LEMA_WORKSPACE_ID pin,
// the sync derives the workspace from the run's git remote — owner/repo →
// slug (owner-repo) → the credential's own listing → the id — and syncs there.
func TestSyncDerivesWorkspaceFromGitRemote(t *testing.T) {
	resetWorkspaceUUIDCache(t)

	cap := &syncCapture{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workspaces", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"workspaces":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","slug":"lemahq-lema","name":"lemahq/lema"}]}`))
	})
	mux.HandleFunc("POST /workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/runs", func(w http.ResponseWriter, r *http.Request) {
		cap.runCreates++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"run":{"id":"11111111-1111-1111-1111-111111111111"},"created":true,"rung":7}`))
	})
	mux.HandleFunc("POST /workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/runs/11111111-1111-1111-1111-111111111111/events", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		cap.events = append(cap.events, req)
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("LEMA_API_URL", srv.URL)
	t.Setenv("LEMA_API_TOKEN", "tok")
	t.Setenv("LEMA_WORKSPACE_ID", "") // no pin — derive
	restoreGit := gitRemoteURL
	t.Cleanup(func() { gitRemoteURL = restoreGit })
	gitRemoteURL = func(string) (string, bool) { return "git@github.com:lemahq/lema.git", true }

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-derive")
	syncOnBoundary(dir, "claude-code", mkEnv("sess-derive", "stop", nil))
	if cap.runCreates != 1 || len(cap.events) != 1 {
		t.Fatalf("derived workspace must resolve and sync: creates=%d events=%d", cap.runCreates, len(cap.events))
	}
}

// The env pin overrides WORKSPACE derivation: when LEMA_WORKSPACE_ID is set the
// workspace resolves from the pin, not the git remote (decision d_d9caf0). The
// run's repo/branch are a SEPARATE concern (repo-on-run-create, 5025ffb7) — they
// are still derived from git even under the pin, because the pin targets the
// corpus, not which repo the run is on (the lema dogfood pins the workspace, and
// its runs must still associate).
func TestSyncEnvPinOverridesDerivation(t *testing.T) {
	cap := &syncCapture{}
	srv := newSyncTestServer(t, cap, http.StatusCreated)
	defer srv.Close()
	setSyncEnv(t, srv.URL) // pins LEMA_WORKSPACE_ID to the aaaa… UUID

	restoreRemote, restoreBranch := gitRemoteURL, gitCurrentBranch
	t.Cleanup(func() { gitRemoteURL, gitCurrentBranch = restoreRemote, restoreBranch })
	gitRemoteURL = func(string) (string, bool) { return "git@github.com:someone/else.git", true }
	gitCurrentBranch = func(string) (string, bool) { return "topic", true }

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-pin")
	syncOnBoundary(dir, "claude-code", mkEnv("sess-pin", "stop", nil))

	// The pin won for the WORKSPACE: the sync hit the pinned aaaa… path (the only
	// handler registered), so the checkpoint landed rather than 404ing on a
	// derived-slug path.
	if len(cap.events) != 1 {
		t.Fatalf("the pinned workspace must sync: events=%d", len(cap.events))
	}
	// ...but the run's repo/branch still come from git — orthogonal to the pin.
	if cap.runCreate["repo"] != "someone/else" || cap.runCreate["branch"] != "topic" {
		t.Fatalf("repo-on-run-create must derive repo/branch even under the workspace pin, got repo=%q branch=%q",
			cap.runCreate["repo"], cap.runCreate["branch"])
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

// The dogfood-found bug (2026-07-21): the authed API parses the workspace
// path param as a UUID, but the configured value is commonly a SLUG — the
// first live sync 400'd silently. The syncer must resolve slug→id via the
// credential's own workspace listing; a workspace the credential cannot see
// resolves to nothing and NOTHING syncs (this is also what stops a
// wrong-org token from writing anywhere).
func TestSyncResolvesSlugWorkspace(t *testing.T) {
	resetWorkspaceUUIDCache(t)

	cap := &syncCapture{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workspaces", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"workspaces":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","slug":"lemahq-lema","name":"lemahq/lema"}]}`))
	})
	mux.HandleFunc("POST /workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/runs", func(w http.ResponseWriter, r *http.Request) {
		cap.runCreates++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"run":{"id":"11111111-1111-1111-1111-111111111111"},"created":true,"rung":7}`))
	})
	mux.HandleFunc("POST /workspaces/11111111-1111-1111-1111-111111111111/events", func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected path")
	})
	mux.HandleFunc("POST /workspaces/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa/runs/11111111-1111-1111-1111-111111111111/events", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		cap.events = append(cap.events, req)
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("LEMA_API_URL", srv.URL)
	t.Setenv("LEMA_API_TOKEN", "tok")
	t.Setenv("LEMA_WORKSPACE_ID", "lemahq-lema") // the slug, exactly as configured in the wild

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-slug")
	syncOnBoundary(dir, "claude-code", mkEnv("sess-slug", "stop", nil))
	if cap.runCreates != 1 || len(cap.events) != 1 {
		t.Fatalf("slug must resolve and sync: creates=%d events=%d", cap.runCreates, len(cap.events))
	}
}

func TestSyncSkipsInvisibleWorkspace(t *testing.T) {
	resetWorkspaceUUIDCache(t)

	hits := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workspaces", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"workspaces":[{"id":"other","slug":"someone-else"}]}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/workspaces" {
			hits++
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("LEMA_API_URL", srv.URL)
	t.Setenv("LEMA_API_TOKEN", "vestige-tok")
	t.Setenv("LEMA_WORKSPACE_ID", "lemahq-lema")

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-wrongorg")
	syncOnBoundary(dir, "claude-code", mkEnv("sess-wrongorg", "stop", nil))
	if hits != 0 {
		t.Fatalf("a workspace this credential cannot see must sync NOTHING, got %d writes", hits)
	}
}

func TestLooksLikeUUID(t *testing.T) {
	if !looksLikeUUID("11111111-1111-1111-1111-111111111111") {
		t.Fatal("canonical uuid must pass")
	}
	for _, bad := range []string{"lemahq-lema", "", "11111111-1111-1111-1111-11111111111Z", "111111111111111111111111111111111111"} {
		if looksLikeUUID(bad) {
			t.Fatalf("%q must not pass", bad)
		}
	}
}
