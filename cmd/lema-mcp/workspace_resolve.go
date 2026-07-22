package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
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

// workspace_resolve.go owns the authenticated HTTP adapters used by target
// resolution. Listing visibility, project links, and repository metadata are
// authority evidence; callers must not infer a destination from listing count.

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
		// org_id is authority evidence, not optional display metadata. A missing
		// value must never make two resources look same-Organization by accident.
		if strings.TrimSpace(entry.OrgID) == "" {
			continue
		}
		workspace := targetWorkspace{
			ID:             entry.ID,
			Slug:           entry.Slug,
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
// resolver. Repository-scoped hosted writes share this provider through their
// injected runtime.
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
// its listing). It remains for collector sync and get_state_brief; hosted write
// operations use the immutable target receipt resolver instead.
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
