package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// makeTestRecords builds n valid capture records with distinct ids
// (d_000001...), the minimum shape the server-side validator accepts.
func makeTestRecords(n int) []source.DecisionRecord {
	recs := make([]source.DecisionRecord, n)
	for i := range recs {
		recs[i] = source.DecisionRecord{
			ID:     fmt.Sprintf("d_%06d", i+1),
			TS:     "2026-06-10T00:00:00Z",
			Title:  fmt.Sprintf("Use option A for thing %d", i+1),
			Chosen: "option A",
			Status: "accepted",
		}
	}
	return recs
}

// The client batches records, authenticates with the bearer token, targets
// the workspace path, and aggregates per-batch counts — the wire half of the
// "one command, idempotent, safe from anywhere" promise.
func TestPushClientBatchesAndAggregates(t *testing.T) {
	type seen struct {
		path          string
		auth          string
		contentType   string
		schemaVersion int
		batchSize     int
	}
	var calls []seen

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SchemaVersion int                     `json:"schema_version"`
			Records       []source.DecisionRecord `json:"records"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		calls = append(calls, seen{
			path:          r.URL.Path,
			auth:          r.Header.Get("Authorization"),
			contentType:   r.Header.Get("Content-Type"),
			schemaVersion: req.SchemaVersion,
			batchSize:     len(req.Records),
		})
		results := make([]map[string]any, len(req.Records))
		for i, rec := range req.Records {
			results[i] = map[string]any{"local_id": rec.ID, "title": rec.Title, "status": "created"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": len(req.Records),
			"results": results,
		})
	}))
	defer srv.Close()

	records := makeTestRecords(3)
	agg, err := pushRecords(context.Background(), srv.URL, "tok123", "ws-uuid", records, 2, false)
	if err != nil {
		t.Fatalf("pushRecords: %v", err)
	}

	if agg.Created != 3 {
		t.Errorf("Created = %d, want 3", agg.Created)
	}
	if len(agg.Results) != 3 {
		t.Errorf("len(Results) = %d, want 3", len(agg.Results))
	}
	if len(calls) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(calls))
	}
	if calls[0].batchSize != 2 || calls[1].batchSize != 1 {
		t.Errorf("batch sizes = [%d %d], want [2 1]", calls[0].batchSize, calls[1].batchSize)
	}
	for i, c := range calls {
		if c.path != "/workspaces/ws-uuid/import-decisions" {
			t.Errorf("call %d path = %q, want /workspaces/ws-uuid/import-decisions", i, c.path)
		}
		if c.auth != "Bearer tok123" {
			t.Errorf("call %d Authorization = %q, want Bearer tok123", i, c.auth)
		}
		if c.contentType != "application/json" {
			t.Errorf("call %d Content-Type = %q, want application/json", i, c.contentType)
		}
		if c.schemaVersion != 1 {
			t.Errorf("call %d schema_version = %d, want 1", i, c.schemaVersion)
		}
	}
}

// A failed batch must tell the user WHY: the wrapped error carries both the
// HTTP status and the server's JSON error message, not just a status line.
func TestPushClientSurfacesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"not a workspace member"}`))
	}))
	defer srv.Close()

	_, err := pushRecords(context.Background(), srv.URL, "tok123", "ws-uuid", makeTestRecords(1), 500, false)
	if err == nil {
		t.Fatal("pushRecords: want error, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q does not mention status 403", err)
	}
	if !strings.Contains(err.Error(), "not a workspace member") {
		t.Errorf("error %q does not carry the server's message", err)
	}
}

// dry_run rides the body only when set — omitted means a real import, so the
// key must be absent (omitempty), not false.
func TestPushClientDryRunFlag(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("decode body: %v", err)
		}
		bodies = append(bodies, m)
		_, _ = w.Write([]byte(`{"created":0,"results":[]}`))
	}))
	defer srv.Close()

	if _, err := pushRecords(context.Background(), srv.URL, "tok", "ws", makeTestRecords(1), 500, true); err != nil {
		t.Fatalf("pushRecords dry-run: %v", err)
	}
	if _, err := pushRecords(context.Background(), srv.URL, "tok", "ws", makeTestRecords(1), 500, false); err != nil {
		t.Fatalf("pushRecords real: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(bodies))
	}
	if v, ok := bodies[0]["dry_run"]; !ok || v != true {
		t.Errorf("dry-run body dry_run = %v (present=%v), want true", v, ok)
	}
	if _, ok := bodies[1]["dry_run"]; ok {
		t.Errorf("real-run body still carries dry_run key: %v", bodies[1]["dry_run"])
	}
}

// Config persists workspace + api url (NEVER the token) so the second run is
// just `lema-mcp push`. The token stays env-only by design.
func TestPushConfigRoundTripAndNoToken(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".lema")

	want := pushConfig{Workspace: "ws-uuid", APIURL: "https://api.lema.sh"}
	if err := savePushConfig(dir, want); err != nil {
		t.Fatalf("savePushConfig: %v", err)
	}
	got, err := loadPushConfig(dir)
	if err != nil {
		t.Fatalf("loadPushConfig: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "push.json"))
	if err != nil {
		t.Fatalf("read config file: %v", err)
	}
	if strings.Contains(strings.ToLower(string(raw)), "token") {
		t.Errorf("config file mentions a token; it must never hold secrets: %s", raw)
	}

	empty, err := loadPushConfig(t.TempDir())
	if err != nil {
		t.Fatalf("loadPushConfig on missing file: %v", err)
	}
	if empty != (pushConfig{}) {
		t.Errorf("missing file config = %+v, want zero value", empty)
	}
}

// The clamp in pushRecords is a backstop for non-CLI callers: an out-of-range
// batch size (0, or absurdly large) must clamp to the 500 cap and still push —
// never panic or loop. User-facing validation lives in runPush instead.
func TestPushClientClampsBatchSize(t *testing.T) {
	for _, batchSize := range []int{0, 9999} {
		var batchSizes []int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Records []source.DecisionRecord `json:"records"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode request: %v", err)
			}
			batchSizes = append(batchSizes, len(req.Records))
			_, _ = w.Write([]byte(`{"created":0,"results":[]}`))
		}))

		_, err := pushRecords(context.Background(), srv.URL, "tok", "ws", makeTestRecords(3), batchSize, false)
		srv.Close()
		if err != nil {
			t.Fatalf("batchSize=%d: pushRecords: %v", batchSize, err)
		}
		if len(batchSizes) != 1 || batchSizes[0] != 3 {
			t.Errorf("batchSize=%d: recorded batches = %v, want [3] (clamped to the %d cap)", batchSize, batchSizes, pushMaxBatch)
		}
	}
}

// seedTestStore creates a capture store at dir/.lema/decisions.jsonl with n
// records (via the real Record path, so ids/timestamps are genuine) and
// returns the store path.
func seedTestStore(t *testing.T, dir string, n int) string {
	t.Helper()
	path := filepath.Join(dir, ".lema", "decisions.jsonl")
	store, err := source.NewCaptureStore(path)
	if err != nil {
		t.Fatalf("NewCaptureStore: %v", err)
	}
	for i := 0; i < n; i++ {
		_, err := store.Record(source.DecisionRecord{
			Title:  fmt.Sprintf("Use option A for thing %d", i+1),
			Chosen: fmt.Sprintf("option A%d", i+1),
		})
		if err != nil {
			t.Fatalf("Record %d: %v", i+1, err)
		}
	}
	return path
}

// importFakeServer answers like the hosted import endpoint: every record comes
// back "created", counts match. Returns the server and a pointer to the
// request count.
func importFakeServer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	requests := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests++
		var req struct {
			Records []source.DecisionRecord `json:"records"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		results := make([]map[string]any, len(req.Records))
		for i, rec := range req.Records {
			results[i] = map[string]any{"local_id": rec.ID, "title": rec.Title, "status": "created"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"created": len(req.Records), "results": results})
	}))
	t.Cleanup(srv.Close)
	return srv, requests
}

// The full first-run → second-run story: push with explicit flags succeeds,
// the workspace+url pair is remembered (never the token), and a bare re-run
// with no flags works off the remembered config alone.
func TestRunPushEndToEndAgainstFakeServer(t *testing.T) {
	dir := t.TempDir()
	captureFile := seedTestStore(t, dir, 2)
	srv, requests := importFakeServer(t)
	t.Setenv("LEMA_API_TOKEN", "tok-e2e")
	t.Setenv("LEMA_API_URL", "") // the remembered config, not a leaked env var, must carry run two

	if err := runPush([]string{"--capture-file", captureFile, "--workspace", "ws-e2e", "--api-url", srv.URL}); err != nil {
		t.Fatalf("first runPush: %v", err)
	}
	if *requests != 1 {
		t.Errorf("server saw %d requests after first run, want 1", *requests)
	}

	raw, err := os.ReadFile(filepath.Join(dir, ".lema", "push.json"))
	if err != nil {
		t.Fatalf("config not persisted: %v", err)
	}
	var cfg pushConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse persisted config: %v", err)
	}
	if cfg.Workspace != "ws-e2e" || cfg.APIURL != srv.URL {
		t.Errorf("persisted config = %+v, want workspace ws-e2e + api url %s", cfg, srv.URL)
	}
	if strings.Contains(string(raw), "tok-e2e") || strings.Contains(strings.ToLower(string(raw)), "token") {
		t.Errorf("persisted config leaks the token: %s", raw)
	}

	// Second run: no --workspace, no --api-url — remembered config only.
	if err := runPush([]string{"--capture-file", captureFile}); err != nil {
		t.Fatalf("second runPush (remembered config): %v", err)
	}
	if *requests != 2 {
		t.Errorf("server saw %d requests after second run, want 2", *requests)
	}
}

// Every missing prerequisite must name its fix: the env var to export, the
// flag to pass, the valid range. A bare error would strand a first-time user.
func TestRunPushActionableErrors(t *testing.T) {
	dir := t.TempDir()
	captureFile := seedTestStore(t, dir, 1)
	t.Setenv("LEMA_API_TOKEN", "")
	t.Setenv("LEMA_API_URL", "")

	err := runPush([]string{"--capture-file", captureFile, "--workspace", "ws", "--api-url", "http://127.0.0.1:1"})
	if err == nil || !strings.Contains(err.Error(), "LEMA_API_TOKEN") {
		t.Errorf("missing token error = %v, want a mention of LEMA_API_TOKEN", err)
	}

	err = runPush([]string{"--capture-file", captureFile})
	if err == nil || !strings.Contains(err.Error(), "--workspace") {
		t.Errorf("missing workspace error = %v, want a mention of --workspace", err)
	}

	t.Setenv("LEMA_API_TOKEN", "tok")
	t.Setenv("LEMA_API_URL", "")
	err = runPush([]string{"--capture-file", captureFile, "--workspace", "ws"})
	if err == nil || !strings.Contains(err.Error(), "--api-url") {
		t.Errorf("missing api-url error = %v, want a mention of --api-url", err)
	}

	for _, size := range []string{"0", "501"} {
		err = runPush([]string{"--capture-file", captureFile, "--batch-size", size})
		if err == nil || !strings.Contains(err.Error(), "between 1 and 500") {
			t.Errorf("--batch-size %s error = %v, want the valid range named", size, err)
		}
	}
}

// An empty (or never-created) store is a normal state, not a failure — push
// reports there is nothing to do and never contacts the server.
func TestRunPushEmptyStoreIsNotAnError(t *testing.T) {
	srv, requests := importFakeServer(t)
	t.Setenv("LEMA_API_TOKEN", "tok")

	captureFile := filepath.Join(t.TempDir(), ".lema", "decisions.jsonl")
	err := runPush([]string{"--capture-file", captureFile, "--workspace", "ws", "--api-url", srv.URL})
	if err != nil {
		t.Fatalf("runPush on empty store: %v", err)
	}
	if *requests != 0 {
		t.Errorf("server saw %d requests, want 0 — nothing to push must mean no network call", *requests)
	}
}

// Server-reported per-record failures must surface as a nonzero exit (an
// error), not vanish inside a "pushed" summary — cron and CI key off it.
// A fully-failed push must also leave no push.json behind: that workspace/url
// pair is unproven and must not become the default for subsequent runs.
func TestRunPushFailedRecordsExitNonzero(t *testing.T) {
	dir := t.TempDir()
	captureFile := seedTestStore(t, dir, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"created":0,"failed":1,"results":[{"local_id":"d_1","title":"t","status":"failed","reason":"invalid status"}]}`))
	}))
	defer srv.Close()
	t.Setenv("LEMA_API_TOKEN", "tok")

	err := runPush([]string{"--capture-file", captureFile, "--workspace", "ws", "--api-url", srv.URL})
	if err == nil || !strings.Contains(err.Error(), "1 record(s) failed") {
		t.Errorf("runPush with a failed record = %v, want '1 record(s) failed'", err)
	}

	// A fully-failed push must not persist push.json — the pair is unproven.
	if _, statErr := os.Stat(filepath.Join(dir, ".lema", "push.json")); !os.IsNotExist(statErr) {
		t.Errorf("fully-failed push created push.json (stat err = %v); config must not be saved when every record fails", statErr)
	}
}

// A push where at least one record succeeds (even alongside failures) must
// persist push.json — that workspace/url pair has been proven to reach the
// server and should become the default for subsequent runs.
func TestRunPushPartialSuccessSavesConfig(t *testing.T) {
	dir := t.TempDir()
	captureFile := seedTestStore(t, dir, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// One record created, one failed — partial success.
		_, _ = w.Write([]byte(`{"created":1,"failed":1,"results":[` +
			`{"local_id":"d_1","title":"t1","status":"created"},` +
			`{"local_id":"d_2","title":"t2","status":"failed","reason":"schema drift"}` +
			`]}`))
	}))
	defer srv.Close()
	t.Setenv("LEMA_API_TOKEN", "tok")

	// Partial push still returns an error (failed > 0), but config must be saved.
	_ = runPush([]string{"--capture-file", captureFile, "--workspace", "ws-partial", "--api-url", srv.URL})

	raw, err := os.ReadFile(filepath.Join(dir, ".lema", "push.json"))
	if err != nil {
		t.Fatalf("push.json not written after partial success: %v", err)
	}
	var cfg pushConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse push.json: %v", err)
	}
	if cfg.Workspace != "ws-partial" || cfg.APIURL != srv.URL {
		t.Errorf("persisted config = %+v, want workspace ws-partial + api url %s", cfg, srv.URL)
	}
}

// A dry run must leave no trace: remembering the workspace/url pair is the
// reward for a real push, not for a preview that wrote nothing server-side.
func TestRunPushDryRunDoesNotSaveConfig(t *testing.T) {
	dir := t.TempDir()
	captureFile := seedTestStore(t, dir, 1)
	srv, _ := importFakeServer(t)
	t.Setenv("LEMA_API_TOKEN", "tok")

	if err := runPush([]string{"--capture-file", captureFile, "--workspace", "ws", "--api-url", srv.URL, "--dry-run"}); err != nil {
		t.Fatalf("runPush --dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".lema", "push.json")); !os.IsNotExist(err) {
		t.Errorf("dry run created push.json (stat err = %v); config must only persist after a real push", err)
	}
}
