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

type collectorRoutingProvider struct {
	receipts map[string]targetContext
	calls    int
}

func (p *collectorRoutingProvider) Resolve(_ context.Context, input resolveTargetInput) (resolutionResult, error) {
	p.calls++
	receipt, ok := p.receipts[input.CWD]
	if !ok {
		return resolutionResult{Status: resolutionUnresolved}, nil
	}
	return resolutionResult{Status: resolutionResolved, Context: receipt}, nil
}

func newSyncTestServer(t *testing.T, cap *syncCapture, eventStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workspaces", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token on workspace validation")
		}
		_, _ = w.Write([]byte(`{"workspaces":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","org_id":"org-1","is_repo":true,"repo_url":"https://github.com/acme/proj.git"}]}`))
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
	}, "/repo/proj", collectorCheckpoint{})
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

// Collector Runs are project-scoped lifecycle state. A frontend session and an
// API session can therefore converge on the same Work Unit under the Project,
// while the Run body preserves which repository each harness actually touched.
func TestCollectorSyncHomesCrossRepositoryRunsOnOneProject(t *testing.T) {
	const (
		projectID    = "11111111-1111-1111-1111-111111111111"
		frontendID   = "22222222-2222-2222-2222-222222222222"
		apiID        = "33333333-3333-3333-3333-333333333333"
		frontendRun  = "44444444-4444-4444-4444-444444444444"
		apiRun       = "55555555-5555-5555-5555-555555555555"
		sharedWorkID = "77777777-7777-7777-7777-777777777777"
	)
	type create struct {
		path string
		body map[string]string
	}
	var creates []create
	var eventPaths []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workspaces/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path[len(r.URL.Path)-5:] == "/runs" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			creates = append(creates, create{path: r.URL.Path, body: body})
			runID := frontendRun
			if body["external_run_id"] == "api-run" {
				runID = apiRun
			}
			_, _ = w.Write([]byte(`{"run":{"id":"` + runID + `","work_unit_id":"` + sharedWorkID + `"}}`))
			return
		}
		eventPaths = append(eventPaths, r.URL.Path)
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	restoreRemote, restoreBranch := gitRemoteURL, gitCurrentBranch
	t.Cleanup(func() { gitRemoteURL, gitCurrentBranch = restoreRemote, restoreBranch })
	gitRemoteURL = func(cwd string) (string, bool) {
		switch cwd {
		case "/repo/frontend":
			return "https://github.com/acme/frontend.git", true
		case "/repo/api":
			return "https://github.com/acme/api.git", true
		default:
			return "", false
		}
	}
	gitCurrentBranch = func(string) (string, bool) { return "main", true }

	provider := &collectorRoutingProvider{receipts: map[string]targetContext{
		"/repo/frontend": collectorRoutingReceipt(projectID, frontendID, "git:github.com/acme/frontend"),
		"/repo/api":      collectorRoutingReceipt(projectID, apiID, "git:github.com/acme/api"),
	}}
	s := &collectorSyncer{
		apiURL: srv.URL, token: "tok", client: srv.Client(),
		runtime: &hostedWriteRuntime{client: srv.Client(), apiURL: srv.URL, token: "tok", targets: provider},
	}
	ctx := context.Background()
	for _, cp := range []collectorCheckpoint{
		{CWD: "/repo/frontend", RunID: "frontend-run", Summary: "frontend", UpdatedAt: "2026-07-22T00:00:00Z"},
		{CWD: "/repo/api", RunID: "api-run", Summary: "api", UpdatedAt: "2026-07-22T00:00:00Z"},
	} {
		if err := s.syncCheckpoint(ctx, "claude-code", cp); err != nil {
			t.Fatal(err)
		}
	}
	var decoded [2][2]string
	for i, in := range []struct {
		externalRunID string
		cwd           string
	}{{"frontend-run", "/repo/frontend"}, {"api-run", "/repo/api"}} {
		identity, err := s.ensureRunInWorkspace(ctx, projectID, "claude-code", in.externalRunID, in.cwd)
		if err != nil {
			t.Fatal(err)
		}
		decoded[i][0], decoded[i][1] = identity.ID, identity.WorkUnitID
	}

	wantRunPath := "/workspaces/" + projectID + "/runs"
	if len(creates) != 4 {
		t.Fatalf("Run creates = %d, want two sync creates plus two decoder probes", len(creates))
	}
	for _, create := range creates {
		if create.path != wantRunPath {
			t.Fatalf("Run create path = %q, want %q", create.path, wantRunPath)
		}
		if _, supplied := create.body["work_unit_id"]; supplied {
			t.Fatalf("collector supplied client-generated work_unit_id: %#v", create.body)
		}
	}
	wantEventPaths := []string{wantRunPath + "/" + frontendRun + "/events", wantRunPath + "/" + apiRun + "/events"}
	if len(eventPaths) != 2 || eventPaths[0] != wantEventPaths[0] || eventPaths[1] != wantEventPaths[1] {
		t.Fatalf("Run event paths = %v, want %v", eventPaths, wantEventPaths)
	}
	if creates[0].body["repo"] != "acme/frontend" || creates[1].body["repo"] != "acme/api" {
		t.Fatalf("Run create repository provenance = %#v, want frontend then api", creates)
	}
	if creates[0].body["external_run_id"] != "frontend-run" || creates[1].body["external_run_id"] != "api-run" {
		t.Fatalf("external Run IDs = %#v, want distinct frontend-run then api-run", creates)
	}
	if decoded[0][0] != frontendRun || decoded[1][0] != apiRun || decoded[0][0] == decoded[1][0] {
		t.Fatalf("decoded Run IDs = %v, want distinct frontend=%s api=%s", decoded, frontendRun, apiRun)
	}
	if decoded[0][1] != sharedWorkID || decoded[1][1] != sharedWorkID {
		t.Fatalf("decoded Work Unit IDs = %v, want shared %s", decoded, sharedWorkID)
	}
	if provider.calls != 2 {
		t.Fatalf("target resolutions = %d, want one immutable receipt per sync", provider.calls)
	}
}

func collectorRoutingReceipt(projectID, repositoryID, canonical string) targetContext {
	receipt := validRoutingContext()
	receipt.ProjectWorkspaceID = projectID
	receipt.RepositoryWorkspaceID = repositoryID
	receipt.VisibleRepositoryWorkspaceIDs = []string{repositoryID}
	receipt.Repository.Canonical = canonical
	receipt.Evidence = []resolutionEvidence{{Kind: "canonical_remote", Value: canonical}}
	return receipt
}

func TestCollectorSyncUsesSingletonRepositoryLeafForBothRunRoutes(t *testing.T) {
	const leafID = "55555555-5555-5555-5555-555555555555"
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/workspaces/"+leafID+"/runs" {
			_, _ = w.Write([]byte(`{"run":{"id":"66666666-6666-6666-6666-666666666666"}}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: collectorRoutingReceipt(leafID, leafID, "git:github.com/acme/singleton")}}
	s := &collectorSyncer{
		apiURL: server.URL, token: "tok", client: server.Client(),
		runtime: &hostedWriteRuntime{client: server.Client(), apiURL: server.URL, token: "tok", targets: provider},
	}
	cp := collectorCheckpoint{CWD: "/repo/singleton", RunID: "singleton-run", Summary: "singleton", UpdatedAt: "2026-07-22T00:00:00Z"}
	if err := s.syncCheckpoint(context.Background(), "claude-code", cp); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/workspaces/" + leafID + "/runs",
		"/workspaces/" + leafID + "/runs/66666666-6666-6666-6666-666666666666/events",
	}
	if len(paths) != len(want) || paths[0] != want[0] || paths[1] != want[1] {
		t.Fatalf("singleton routes = %v, want %v", paths, want)
	}
}

func TestCollectorSyncNonResolvedOrMalformedReceiptPostsNothing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result resolutionResult
	}{
		{"unresolved", resolutionResult{Status: resolutionUnresolved}},
		{"ambiguous", resolutionResult{Status: resolutionAmbiguous}},
		{"forbidden", resolutionResult{Status: resolutionForbidden}},
		{"stale", resolutionResult{Status: resolutionStale}},
		{"malformed", resolutionResult{Status: resolutionResolved, Context: targetContext{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			provider := &fakeTargetProvider{result: tc.result}
			s := &collectorSyncer{
				apiURL: server.URL, token: "tok", client: server.Client(),
				runtime: &hostedWriteRuntime{client: server.Client(), apiURL: server.URL, token: "tok", targets: provider},
			}
			err := s.syncCheckpoint(context.Background(), "claude-code", collectorCheckpoint{CWD: "/repo/no-route", RunID: "no-route"})
			if err == nil {
				t.Fatal("non-resolved receipt must reject collector sync")
			}
			if provider.calls != 1 || requests != 0 {
				t.Fatalf("provider calls=%d operation HTTP=%d, want 1/0", provider.calls, requests)
			}
		})
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

// With no explicit pin, sync uses the shared resolver's canonical Git evidence
// to select a visible repository receipt. This must exercise the real Git
// reader: the collector-specific remote-to-slug adapter no longer exists.
func TestSyncResolvesTargetFromCanonicalGitRemote(t *testing.T) {
	resetWorkspaceUUIDCache(t)

	cap := &syncCapture{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workspaces", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"workspaces":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","org_id":"org-1","slug":"lemahq-lema","name":"lemahq/lema","is_repo":true,"repo_url":"https://github.com/lemahq/lema.git"}]}`))
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
	t.Setenv("LEMA_WORKSPACE_ID", "")
	root := t.TempDir()
	gitHere(t, root, "init")
	gitHere(t, root, "remote", "add", "origin", "https://github.com/lemahq/lema.git")

	dir := t.TempDir()
	cp := distillEnvelopes([]collectorEnvelope{mkEnv("sess-derive", "user_prompt", map[string]string{"prompt": "sync me"})}, root, collectorCheckpoint{})
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	ev := mkEnv("sess-derive", "stop", nil)
	ev.Evidence["cwd"] = root
	syncOnBoundary(dir, "claude-code", ev)
	if cap.runCreates != 1 || len(cap.events) != 1 {
		t.Fatalf("canonical Git target must resolve and sync: creates=%d events=%d", cap.runCreates, len(cap.events))
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
	}, "/repo/proj", collectorCheckpoint{})
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
		_, _ = w.Write([]byte(`{"workspaces":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","org_id":"org-1","slug":"lemahq-lema","name":"lemahq/lema","is_repo":true,"repo_url":"https://github.com/lemahq/lema.git"}]}`))
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
