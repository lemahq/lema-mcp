package main

import (
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
	agg, err := pushRecords(srv.URL, "tok123", "ws-uuid", records, 2, false)
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

	_, err := pushRecords(srv.URL, "tok123", "ws-uuid", makeTestRecords(1), 500, false)
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

	if _, err := pushRecords(srv.URL, "tok", "ws", makeTestRecords(1), 500, true); err != nil {
		t.Fatalf("pushRecords dry-run: %v", err)
	}
	if _, err := pushRecords(srv.URL, "tok", "ws", makeTestRecords(1), 500, false); err != nil {
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
