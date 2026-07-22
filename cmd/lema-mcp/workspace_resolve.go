package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
)

// targetResolutionError retains the only diagnostic distinction callers may
// safely expose: authorization failure versus an unresolved lookup. It never
// carries a response body, endpoint, credential, or local path.
type targetResolutionError struct {
	status resolutionStatus
	rung   string
}

func (e *targetResolutionError) Error() string {
	return "target lookup " + string(e.status)
}

func targetResolutionStatusFromError(err error) resolutionStatus {
	var typed *targetResolutionError
	if errors.As(err, &typed) {
		return typed.status
	}
	return resolutionUnresolved
}

func targetResolutionRungFromError(err error) string {
	var typed *targetResolutionError
	if errors.As(err, &typed) && typed.rung != "" {
		return typed.rung
	}
	return "workspace_lookup"
}

// workspace_resolve.go — graceful recovery for a hosted capture with no
// configured target workspace (#348). record_decision used to hard-fail the
// moment LEMA_WORKSPACE_ID was unset, with no discoverable path to the id (the
// only fix was a human URL-hunt in the web app, then an MCP server restart) —
// a sibling session lost four captures to it. That breaks the capture tenet:
// the record only stays complete if recording costs nothing at the moment a
// decision lands. So instead of failing, the recorder asks the API which
// workspaces this credential can actually see (GET /workspaces — the same
// listing the web app renders):
//
//   - exactly one active workspace -> resolve to it and push, saying so in the
//     tool response (with the env var that pins it, for when a second
//     workspace appears later);
//   - several -> error listing each workspace's slug + id and where to set
//     LEMA_WORKSPACE_ID — self-serve, no web-app spelunking (never guess: a
//     capture landing in the wrong team's corpus is worse than a retry);
//   - none, or the listing fails -> an actionable error naming the manual
//     path. A transient listing failure is NOT memoized (the next capture
//     retries); a successful resolution IS (steady state costs one GET total).
//
// An explicitly configured LEMA_WORKSPACE_ID (env, credentials file, or
// .mcp.json) always wins — main() only wires this resolver when
// resolveWorkspaceID() came back empty.

// workspaceEntry is the slice of the /workspaces listing the resolver needs.
type workspaceEntry struct {
	ID         string  `json:"id"`
	OrgID      string  `json:"org_id"`
	Name       string  `json:"name"`
	Slug       string  `json:"slug"`
	RepoURL    string  `json:"repo_url"`
	IsRepo     bool    `json:"is_repo"`
	ArchivedAt *string `json:"archived_at"`
}

// label names a workspace for humans/agents: slug first (what the web app's
// URLs show), then name, then the raw id.
func (w workspaceEntry) label() string {
	if w.Slug != "" {
		return w.Slug
	}
	if w.Name != "" {
		return w.Name
	}
	return w.ID
}

// fetchWorkspaces lists the workspaces visible to this credential — the same
// GET /workspaces the web app uses, with the same bearer token the push uses.
func fetchWorkspaces(ctx context.Context, client *http.Client, apiURL, token string) ([]workspaceEntry, error) {
	url := strings.TrimRight(apiURL, "/") + "/workspaces"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &targetResolutionError{status: resolutionUnresolved, rung: "workspace_lookup"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, &targetResolutionError{status: resolutionUnresolved, rung: "workspace_lookup"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &targetResolutionError{status: resolutionForbidden, rung: "workspace_lookup"}
		}
		return nil, &targetResolutionError{status: resolutionUnresolved, rung: "workspace_lookup"}
	}
	var out struct {
		Workspaces []workspaceEntry `json:"workspaces"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &targetResolutionError{status: resolutionUnresolved, rung: "workspace_lookup"}
	}
	return out.Workspaces, nil
}

// fetchWorkspaceLinks loads the repository leaves linked to one visible
// non-repository container. The server omits leaves the credential cannot see;
// callers still intersect the returned ids with their own visible listing.
func fetchWorkspaceLinks(ctx context.Context, client *http.Client, apiURL, token, workspaceID string) ([]string, error) {
	url := strings.TrimRight(apiURL, "/") + "/workspaces/" + workspaceID + "/links"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, &targetResolutionError{status: resolutionUnresolved, rung: "project_link_lookup"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, &targetResolutionError{status: resolutionUnresolved, rung: "project_link_lookup"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, &targetResolutionError{status: resolutionForbidden, rung: "project_link_lookup"}
		}
		return nil, &targetResolutionError{status: resolutionUnresolved, rung: "project_link_lookup"}
	}
	var out struct {
		Links []struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"links"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, &targetResolutionError{status: resolutionUnresolved, rung: "project_link_lookup"}
	}
	ids := make([]string, 0, len(out.Links))
	for _, link := range out.Links {
		if link.WorkspaceID != "" {
			ids = append(ids, link.WorkspaceID)
		}
	}
	return ids, nil
}

// targetWorkspacesFromEntries adapts the authoritative visible listing to the
// resolver's transport-free seam. A malformed repository URL remains an
// unmatchable repository rather than becoming a guessed identity.
func targetWorkspacesFromEntries(entries []workspaceEntry) []targetWorkspace {
	workspaces := make([]targetWorkspace, 0, len(entries))
	for _, entry := range entries {
		workspace := targetWorkspace{
			ID:             entry.ID,
			OrganizationID: entry.OrgID,
			IsRepository:   entry.IsRepo,
			Archived:       entry.ArchivedAt != nil,
		}
		if entry.IsRepo {
			identity, ok := repositoryIdentityFromRemote(entry.RepoURL)
			if !ok {
				continue
			}
			workspace.Repository = identity
		}
		workspaces = append(workspaces, workspace)
	}
	return workspaces
}

// newHostedTargetResolver supplies the HTTP adapters for the otherwise pure
// resolver. It intentionally does not route any production operation through
// the resolver yet; later callers can opt in by constructing this adapter.
func newHostedTargetResolver(client *http.Client, apiURL, token string) *targetResolver {
	return &targetResolver{
		fetchWorkspaces: func(ctx context.Context, _ resolveTargetInput) ([]targetWorkspace, error) {
			entries, err := fetchWorkspaces(ctx, client, apiURL, token)
			if err != nil {
				return nil, err
			}
			return targetWorkspacesFromEntries(entries), nil
		},
		fetchLinks: func(ctx context.Context, _ resolveTargetInput, projectID string) ([]string, error) {
			return fetchWorkspaceLinks(ctx, client, apiURL, token, projectID)
		},
		cache: newTargetResolutionCache(64),
	}
}

// resolveWorkspaceValueUUID turns a configured workspace value (a UUID, or a
// slug/id to look up) into the UUID the authed /workspaces/{id}/... path parser
// requires. Every value, including a UUID, is matched against GET /workspaces
// by slug or id, memoized per (API URL, credential fingerprint, value). A value this
// credential cannot see resolves to an error — which is also the guard that
// stops a wrong-org token from writing anywhere (the workspace simply isn't in
// its listing). This is the single implementation shared by the collector sync,
// get_state_brief (both via collectorSyncer.resolveWorkspaceUUID), and the
// record_decision / import-decisions push (pushDecisions), so any repo config —
// slug or UUID — resolves identically. Found by the first live dogfood run
// (2026-07-21): a slug-configured LEMA_WORKSPACE_ID 400'd "invalid workspaceID"
// on the collector path (fixed in a9ca2c5); the push path had the same gap.
func resolveWorkspaceValueUUID(ctx context.Context, client *http.Client, apiURL, token, v string) (string, error) {
	key := strings.Join([]string{strings.TrimRight(apiURL, "/"), credentialFingerprint(token), v}, "\x00")
	cached, ok := workspaceUUIDCache.get(key)
	if ok {
		return cached, nil
	}
	all, err := fetchWorkspaces(ctx, client, apiURL, token)
	if err != nil {
		return "", err
	}
	for _, w := range all {
		if strings.EqualFold(w.Slug, v) || w.ID == v {
			workspaceUUIDCache.put(key, w.ID)
			return w.ID, nil
		}
	}
	return "", &targetResolutionError{status: resolutionStale, rung: "explicit_workspace"}
}

// workspaceIDHint is the one sentence every resolution error carries: where a
// workspace id can be configured. Shared so the guidance cannot drift apart
// across the error paths.
const workspaceIDHint = "set " + workspaceIDEnv + " in your shell env, ~/.config/lema/credentials, or the .mcp.json env block (the id is also in the web app's workspace URL)"

// newWorkspaceAutoResolvingPush returns the record_decision sink for hosted
// mode WITHOUT a configured workspace id: it resolves the target lazily on the
// first capture (memoizing success, retrying after failure) and then pushes
// exactly like the configured-workspace sink, appending an auto-resolution
// note to the tool response so the caller knows where the capture landed.
func newWorkspaceAutoResolvingPush(apiURL, token string, client *http.Client) func(ctx context.Context, dr source.DecisionRecord) (recordOutput, error) {
	var (
		mu       sync.Mutex
		resolved *workspaceEntry
	)
	resolve := func(ctx context.Context) (workspaceEntry, error) {
		mu.Lock()
		defer mu.Unlock()
		if resolved != nil {
			return *resolved, nil
		}
		all, err := fetchWorkspaces(ctx, client, apiURL, token)
		if err != nil {
			return workspaceEntry{}, fmt.Errorf("hosted mode is on but %s is unset, and auto-resolving it failed (%v) — %s", workspaceIDEnv, err, workspaceIDHint)
		}
		active := all[:0:0]
		for _, w := range all {
			if w.ArchivedAt == nil {
				active = append(active, w)
			}
		}
		switch len(active) {
		case 0:
			return workspaceEntry{}, fmt.Errorf("hosted mode is on but %s is unset and this credential can see no active workspace — connect a repo or create a workspace in the lema app, then %s", workspaceIDEnv, workspaceIDHint)
		case 1:
			resolved = &active[0]
			return *resolved, nil
		default:
			labels := make([]string, len(active))
			for i, w := range active {
				labels[i] = fmt.Sprintf("%s (%s)", w.label(), w.ID)
			}
			return workspaceEntry{}, fmt.Errorf("hosted mode is on but %s is unset and this credential can see %d workspaces — %s to one of: %s", workspaceIDEnv, len(active), workspaceIDHint, strings.Join(labels, ", "))
		}
	}
	return func(ctx context.Context, dr source.DecisionRecord) (recordOutput, error) {
		ws, err := resolve(ctx)
		if err != nil {
			return recordOutput{}, fmt.Errorf("record_decision: %w", err)
		}
		out, err := recordToHosted(ctx, dr, time.Now(), func(ctx context.Context, recs []pushRecord) (pushResponse, error) {
			return pushDecisions(ctx, client, apiURL, token, ws.ID, recs)
		})
		if err != nil {
			return recordOutput{}, err
		}
		out.Recorded += fmt.Sprintf(" [workspace auto-resolved to %s — the only workspace this credential can see; set %s to pin it]", ws.label(), workspaceIDEnv)
		return out, nil
	}
}
