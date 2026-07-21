// collector_sync.go is the hosted half of the open collector (pivot B2 F3):
// at a run boundary, the freshly distilled checkpoint syncs to the server's
// run journal — POST /runs (idempotent on harness+external_run_id) then
// POST /runs/{id}/events kind 'checkpoint' (the server compacts prior
// checkpoints in-tx, so re-syncing is safe by design). Credentials resolve
// env-first-then-file exactly like the push/settle paths; missing config,
// a dark flag (404), or any transport failure is a silent local-only
// outcome — the spool remains the truth and a hook is never blocked.
//
// Only the checkpoint syncs in v1: the collector has no producer for the
// server's other client kinds (observation, candidate-claim, conflict,
// handoff) yet, and raw harness envelopes do not belong in that settled
// vocabulary — each kind gains a sync when it gains a real producer.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// collectorSyncTimeout bounds the whole boundary sync (both requests). Kept
// tighter than pushTimeout: this runs inside hook budgets alongside other
// Stop-hook work.
const collectorSyncTimeout = 5 * time.Second

type collectorSyncer struct {
	apiURL      string
	token       string
	workspaceID string
	client      *http.Client
}

// newCollectorSyncer resolves hosted config. URL/token follow the usual
// env-first-then-file precedence (identity is per-user), but the TARGET
// WORKSPACE must come from process env — the project-scoped .mcp.json
// channel — never the per-user credentials file: an ambient hook firing in
// every project would otherwise silently write every project's run state
// into whatever workspace the global file names (the exact wrong-corpus
// foot-gun settle warns about interactively; a hook cannot ask, so it
// refuses instead). nil = not configured — the caller skips silently.
func newCollectorSyncer() *collectorSyncer {
	apiURL, token, _ := resolveHostedConfig()
	workspaceID := strings.TrimSpace(os.Getenv(workspaceIDEnv))
	if apiURL == "" || token == "" || workspaceID == "" {
		return nil
	}
	return &collectorSyncer{
		apiURL: apiURL, token: token, workspaceID: workspaceID,
		client: &http.Client{Timeout: collectorSyncTimeout},
	}
}

// looksLikeUUID reports whether s has the canonical 8-4-4-4-12 hex shape.
// The authed /workspaces/{id}/... group parses the path param as a UUID, so
// a slug-configured LEMA_WORKSPACE_ID must resolve to the id first.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// workspaceUUIDCache memoizes slug→id resolutions per (apiURL, value) so the
// long-lived MCP server pays one listing per target; hook processes are
// one-shot and pay one listing per boundary event, bounded by the 5s budget.
var (
	workspaceUUIDMu    sync.Mutex
	workspaceUUIDCache = map[string]string{}
)

// resolveWorkspaceUUID turns the configured workspace value into the UUID the
// API's path parser requires. Thin receiver-bound wrapper over
// resolveWorkspaceValueUUID — the shared implementation the push path also uses
// (workspace_resolve.go), so slug resolution can never drift apart across paths.
func (s *collectorSyncer) resolveWorkspaceUUID(ctx context.Context) (string, error) {
	return resolveWorkspaceValueUUID(ctx, s.client, s.apiURL, s.token, s.workspaceID)
}

func (s *collectorSyncer) post(ctx context.Context, path string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	url := strings.TrimRight(s.apiURL, "/") + "/workspaces/" + s.workspaceID + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, respBody, nil
}

// ensureRun creates (or re-finds — CreateRun is idempotent on
// harness+external_run_id) the hosted run identity and returns its id.
// v1 sends no repo (the adapter has no owner/name source yet), so the
// server-side association ladder honestly lands these runs at rung 7 —
// rungs 3/4 fire only once the adapter supplies repo (named follow-up).
func (s *collectorSyncer) ensureRun(ctx context.Context, harness, externalRunID, worktree string) (string, error) {
	status, body, err := s.post(ctx, "/runs", map[string]string{
		"harness":         harness,
		"external_run_id": externalRunID,
		"worktree":        worktree,
	})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", fmt.Errorf("create run: HTTP %d", status)
	}
	var out struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Run.ID == "" {
		return "", fmt.Errorf("create run: no id in response")
	}
	return out.Run.ID, nil
}

// syncCheckpoint lands the distilled checkpoint on the hosted run journal.
// Every failure path returns an error the caller ignores (fail-open); the
// server's in-tx subsumption makes repeated syncs safe.
func (s *collectorSyncer) syncCheckpoint(ctx context.Context, harness string, cp collectorCheckpoint) error {
	uuid, err := s.resolveWorkspaceUUID(ctx)
	if err != nil {
		return err
	}
	s.workspaceID = uuid
	runID, err := s.ensureRun(ctx, harness, cp.RunID, cp.CWD)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"summary":     cp.Summary,
		"cwd":         cp.CWD,
		"event_count": cp.EventCount,
		"updated_at":  cp.UpdatedAt,
	}
	if len(cp.RecentPrompts) > 0 {
		payload["recent_prompts"] = cp.RecentPrompts
	}
	if len(cp.FilesTouched) > 0 {
		payload["files_touched"] = cp.FilesTouched
	}
	status, _, err := s.post(ctx, "/runs/"+runID+"/events", map[string]any{
		"kind":    "checkpoint",
		"payload": payload,
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return fmt.Errorf("append checkpoint: HTTP %d", status)
	}
	return nil
}

// syncOnBoundary is runCollect's hook into the hosted half: after a boundary
// event distilled a fresh checkpoint, sync it. Missing config, a dark server
// (404 while lema-run-state is off), and every other failure are silent —
// the local spool stays authoritative.
func syncOnBoundary(dir, harness string, ev collectorEnvelope) {
	switch ev.Kind {
	case "pre_compact", "stop", "session_end":
	default:
		return
	}
	cwd := ev.Evidence["cwd"]
	if cwd == "" {
		return
	}
	s := newCollectorSyncer()
	if s == nil {
		return
	}
	cp, ok := readCollectorCheckpoint(dir, cwd, time.Now())
	if !ok || cp.RunID != ev.RunID {
		// Sync only the checkpoint THIS run just produced — never re-send
		// another run's state under this run's identity.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), collectorSyncTimeout)
	defer cancel()
	_ = s.syncCheckpoint(ctx, harness, cp)
}
