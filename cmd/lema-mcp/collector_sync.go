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
	runtime     *hostedWriteRuntime
}

// newCollectorSyncer constructs the shared write runtime once for callers that
// do not already have checkpoint cwd evidence. Boundary sync uses
// newCollectorSyncerForCWD so a local association is loaded against the
// checkpoint's immutable evidence rather than the hook process's ambient cwd.
func newCollectorSyncer() *collectorSyncer {
	return newCollectorSyncerForCWD("")
}

// newCollectorSyncerForCWD loads credentials once and builds the same hosted
// target provider used by every other state-changing operation. A workspace
// value from the credentials file is intentionally not treated as a collector
// target: only the process environment is an explicit hook override.
func newCollectorSyncerForCWD(cwd string) *collectorSyncer {
	config := resolveHostedWriteConfig()
	if config.APIURL == "" || config.Token == "" {
		return nil
	}
	client := &http.Client{Timeout: collectorSyncTimeout}
	runtime := newHostedWriteRuntime(client, config.APIURL, config.Token, strings.TrimSpace(os.Getenv(workspaceIDEnv)), cwd)
	return &collectorSyncer{
		apiURL:  runtime.apiURL,
		token:   runtime.token,
		client:  runtime.client,
		runtime: &runtime,
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

// workspaceUUIDCache memoizes validated values per API URL, SHA-256 credential
// fingerprint, and configured override so one credential cannot reuse another
// credential's authority result. It is deliberately small and short-lived:
// workspace slugs are mutable authority metadata, not permanent identifiers.
const workspaceUUIDCacheTTL = time.Minute

var workspaceUUIDCache = newWorkspaceUUIDCache(64, workspaceUUIDCacheTTL, time.Now)

type workspaceUUIDCacheEntry struct {
	workspaceID string
	expires     time.Time
}

type workspaceUUIDResolutionCache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	now      func() time.Time
	values   map[string]workspaceUUIDCacheEntry
	order    []string
}

func newWorkspaceUUIDCache(capacity int, ttl time.Duration, now func() time.Time) *workspaceUUIDResolutionCache {
	if capacity < 1 {
		capacity = 1
	}
	if ttl <= 0 {
		ttl = workspaceUUIDCacheTTL
	}
	if now == nil {
		now = time.Now
	}
	return &workspaceUUIDResolutionCache{capacity: capacity, ttl: ttl, now: now, values: make(map[string]workspaceUUIDCacheEntry)}
}

func (c *workspaceUUIDResolutionCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.values[key]
	if !ok {
		return "", false
	}
	if !c.now().Before(entry.expires) {
		c.delete(key)
		return "", false
	}
	return entry.workspaceID, true
}

func (c *workspaceUUIDResolutionCache) put(key, workspaceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.values[key]; !exists {
		c.order = append(c.order, key)
	}
	c.values[key] = workspaceUUIDCacheEntry{workspaceID: workspaceID, expires: c.now().Add(c.ttl)}
	for len(c.order) > c.capacity {
		c.delete(c.order[0])
	}
}

func (c *workspaceUUIDResolutionCache) delete(key string) {
	delete(c.values, key)
	for i, candidate := range c.order {
		if candidate == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func (c *workspaceUUIDResolutionCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.values)
}

// resolveWorkspaceUUID turns the configured workspace value into the UUID the
// API's path parser requires. Thin receiver-bound wrapper over
// resolveWorkspaceValueUUID — the shared implementation the push path also uses
// (workspace_resolve.go), so slug resolution can never drift apart across paths.
func (s *collectorSyncer) resolveWorkspaceUUID(ctx context.Context) (string, error) {
	return resolveWorkspaceValueUUID(ctx, s.client, s.apiURL, s.token, s.workspaceID)
}

func (s *collectorSyncer) post(ctx context.Context, path string, body any) (int, []byte, error) {
	return s.postToWorkspace(ctx, s.workspaceID, path, body)
}

func (s *collectorSyncer) postToWorkspace(ctx context.Context, workspaceID, path string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	url := strings.TrimRight(s.apiURL, "/") + "/workspaces/" + workspaceID + path
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

type collectorRunIdentity struct {
	ID         string
	WorkUnitID string
}

// ensureRun creates (or re-finds — CreateRun is idempotent on
// harness+external_run_id) the hosted run identity and returns its id. It feeds
// the server-side association ladder its rung-3/4 inputs: repo ("owner/name")
// and branch derived from cwd's git context (the same context the workspace
// derivation uses), plus the worktree (= cwd — apt here, where each agent runs
// in its own git worktree). All best-effort: a cwd with no git remote sends
// empty repo/branch and the run lands rung-7 exactly as before (decision
// 5025ffb7, implementing d_d9caf0 — repo-on-run-create).
func (s *collectorSyncer) ensureRun(ctx context.Context, harness, externalRunID, cwd string) (string, error) {
	identity, err := s.ensureRunInWorkspace(ctx, s.workspaceID, harness, externalRunID, cwd)
	return identity.ID, err
}

func (s *collectorSyncer) ensureRunInWorkspace(ctx context.Context, workspaceID, harness, externalRunID, cwd string) (collectorRunIdentity, error) {
	repo, branch := deriveRunGitContext(cwd)
	status, body, err := s.postToWorkspace(ctx, workspaceID, "/runs", map[string]string{
		"harness":         harness,
		"external_run_id": externalRunID,
		"repo":            repo,
		"branch":          branch,
		"worktree":        cwd,
	})
	if err != nil {
		return collectorRunIdentity{}, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return collectorRunIdentity{}, fmt.Errorf("create run: HTTP %d", status)
	}
	var out struct {
		Run struct {
			ID         string `json:"id"`
			WorkUnitID string `json:"work_unit_id"`
		} `json:"run"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Run.ID == "" {
		return collectorRunIdentity{}, fmt.Errorf("create run: no id in response")
	}
	return collectorRunIdentity{ID: out.Run.ID, WorkUnitID: out.Run.WorkUnitID}, nil
}

// syncCheckpoint lands the distilled checkpoint on the hosted run journal.
// Every failure path returns an error the caller ignores (fail-open); the
// server's in-tx subsumption makes repeated syncs safe.
func (s *collectorSyncer) syncCheckpoint(ctx context.Context, harness string, cp collectorCheckpoint) error {
	if s.runtime == nil {
		return fmt.Errorf("collector sync target runtime is not configured")
	}
	input := s.runtime.targetInput
	if input.CWD != cp.CWD {
		// A receipt-local association is only evidence for the cwd that loaded
		// it. A direct test or legacy caller with another cwd must re-resolve
		// from the checkpoint rather than inherit ambient process state.
		input.CWD = cp.CWD
		input.LocalAssociation = nil
		input.PersistedAssociation = false
	}
	_, err := withResolvedTarget(ctx, s.runtime.targets, input, func(ctx context.Context, receipt targetContext) (struct{}, error) {
		run, err := s.ensureRunInWorkspace(ctx, receipt.ProjectWorkspaceID, harness, cp.RunID, cp.CWD)
		if err != nil {
			return struct{}{}, err
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
		status, _, err := s.postToWorkspace(ctx, receipt.ProjectWorkspaceID, "/runs/"+run.ID+"/events", map[string]any{
			"kind":    "checkpoint",
			"payload": payload,
		})
		if err != nil {
			return struct{}{}, err
		}
		if status != http.StatusCreated {
			return struct{}{}, fmt.Errorf("append checkpoint: HTTP %d", status)
		}
		return struct{}{}, nil
	})
	return err
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
		collectorDebugf("sync skipped: %s envelope for run %s carries no cwd evidence", ev.Kind, ev.RunID)
		return
	}
	cp, ok := readCollectorCheckpoint(dir, cwd, time.Now())
	if !ok {
		collectorDebugf("sync skipped: no live checkpoint on disk for %s — absent, unreadable, or older than the %s TTL", cwd, collectorTTL)
		return
	}
	if cp.RunID != ev.RunID {
		// Sync only the checkpoint THIS run just produced — never re-send
		// another run's state under this run's identity.
		collectorDebugf("sync skipped: checkpoint belongs to run %s but this boundary is run %s — refusing to re-send another run's state under this identity", cp.RunID, ev.RunID)
		return
	}
	s := newCollectorSyncerForCWD(cp.CWD)
	if s == nil {
		collectorDebugf("sync skipped: hosted target not configured — LEMA_API_URL and LEMA_API_TOKEN did not resolve from the environment or the credentials file")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), collectorSyncTimeout)
	defer cancel()
	if err := s.syncCheckpoint(ctx, harness, cp); err != nil {
		collectorDebugf("sync failed for run %s: %v — the local spool remains authoritative", ev.RunID, err)
		return
	}
	collectorDebugf("sync ok: checkpoint for run %s landed on the hosted run journal", ev.RunID)
}
