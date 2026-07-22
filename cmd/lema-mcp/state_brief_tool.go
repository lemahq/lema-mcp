// state_brief_tool.go is the hosted-only `get_state_brief` MCP verb (pivot
// B2, PIVOT_SPEC §4): the scoped State Brief for this run — a thin reader of
// the server's GET /workspaces/{id}/brief?run=. Alias-then-deprecate posture:
// the existing tools are untouched; this is the first of the three end-state
// verbs to land.
//
// Run resolution: an explicit hosted run UUID wins; otherwise the verb
// resolves the PRIOR run for this project from the local F4 checkpoint
// (cwd-keyed) and ensures its hosted identity (idempotent on
// harness+external_run_id) — the relay read: a fresh session asks for the
// state it is resuming. No checkpoint and no explicit run = an honest
// "no prior run known", never a fabricated scope.
//
// Workspace resolution matches the REST of the MCP server (env then the
// per-user credentials file): this is an explicitly configured, per-project
// server the operator launched — not the ambient hook, whose env-only rule
// (collector_sync.go) exists because a hook fires everywhere unattended.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type stateBriefInput struct {
	Run string `json:"run,omitempty" jsonschema:"optional hosted run UUID; omit to resolve this project's prior run from the local checkpoint"`
}

// stateBriefOutput passes the server's brief through verbatim (scope,
// sections, silences, as_of) plus a note on how the run was resolved.
//
// Sections and Silences are `any`, NOT json.RawMessage: the go-sdk infers the
// tool's output schema from this struct, and json.RawMessage ([]byte) infers
// as {type: array, items: {type: integer}} — the SDK's own output validation
// then rejected every non-empty brief (0.21.0 regression, caught live). `any`
// infers a permissive schema; the values are the wire JSON decoded 1:1, so
// consumers see the same content.
type stateBriefOutput struct {
	Scope    string `json:"scope,omitempty"`
	Sections any    `json:"sections,omitempty"`
	Silences any    `json:"silences,omitempty"`
	AsOf     string `json:"as_of,omitempty"`
	Note     string `json:"note,omitempty"`
}

var getStateBriefTool = &mcp.Tool{
	Name: "get_state_brief",
	Description: "Returns the scoped State Brief for a run: objective, last checkpoint, files in flight, " +
		"settled decisions in scope (cited), binding rejected approaches, related active runs — " +
		"composed deterministically from the recorded state, with every unavailable section named " +
		"in silences. Omitting run resolves this project's prior session (the relay read).",
	Annotations: readOnlyExternal("Get the State Brief (hosted)"),
}

const stateBriefTimeout = 10 * time.Second

// briefClient reuses the collector syncer's HTTP shape with the MCP server's
// own workspace resolution (see the file header for why they differ).
func newBriefClient() *collectorSyncer {
	apiURL, token, _ := resolveHostedConfig()
	workspaceID := resolveWorkspaceID()
	if apiURL == "" || token == "" || workspaceID == "" {
		return nil
	}
	return &collectorSyncer{
		apiURL: apiURL, token: token, workspaceID: workspaceID,
		client: &http.Client{Timeout: stateBriefTimeout},
	}
}

func (s *collectorSyncer) get(ctx context.Context, path string) (int, []byte, error) {
	url := strings.TrimRight(s.apiURL, "/") + "/workspaces/" + s.workspaceID + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// resolvePriorRun finds this project's prior run via the local F4 checkpoint
// and returns its hosted run UUID (creating the identity idempotently).
func resolvePriorRun(ctx context.Context, s *collectorSyncer) (runID, note string, err error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", fmt.Errorf("cwd unresolvable: %w", err)
	}
	dir, err := collectorDir()
	if err != nil {
		return "", "", err
	}
	cp, ok := readCollectorCheckpoint(dir, cwd, time.Now())
	if !ok {
		// The checkpoint was written from a hook's self-reported cwd, which
		// can differ from this process's Getwd across symlinks — retry with
		// the resolved path before concluding there is no prior run.
		if resolved, rerr := filepath.EvalSymlinks(cwd); rerr == nil && resolved != cwd {
			cp, ok = readCollectorCheckpoint(dir, resolved, time.Now())
		}
	}
	if !ok {
		return "", "", fmt.Errorf("no prior run known for this project (no local checkpoint under %s)", cwd)
	}
	harness := cp.Harness
	if harness == "" {
		// Checkpoints written before the harness field existed: the collector
		// shipped with only this adapter, so the key is stable.
		harness = "claude-code"
	}
	hosted, err := s.ensureRun(ctx, harness, cp.RunID, cp.CWD)
	if err != nil {
		return "", "", err
	}
	return hosted, fmt.Sprintf("resolved from this project's prior session %s (%s)", cp.RunID, harness), nil
}

// stateBrief is the ONE code path the State Brief serves from — workspace
// resolution, the F4 prior-run relay read when run is empty, the GET /brief
// fetch, and the verbatim pass-through. Extracted from the tool handler so
// the lema://brief resource (brief_resource.go, decision fa8a63f4) can wrap
// it thinly with zero drift. caller labels the usage metric with which
// surface served. Every can't-serve path is an honest note in the output,
// never an error — a fresh session should read state, not a failure.
func stateBrief(ctx context.Context, run, caller string) stateBriefOutput {
	s := newBriefClient()
	if s == nil {
		return stateBriefOutput{
			Note: "state brief unavailable: hosted mode is not configured (LEMA_API_URL / LEMA_API_TOKEN / LEMA_WORKSPACE_ID)",
		}
	}
	if wsUUID, err := s.resolveWorkspaceUUID(ctx); err != nil {
		return stateBriefOutput{Note: "state brief unavailable: " + err.Error()}
	} else {
		s.workspaceID = wsUUID
	}
	runID := strings.TrimSpace(run)
	note := "explicit run id"
	if runID == "" {
		var err error
		runID, note, err = resolvePriorRun(ctx, s)
		if err != nil {
			return stateBriefOutput{Note: "state brief unavailable: " + err.Error()}
		}
	}
	status, body, err := s.get(ctx, "/brief?run="+url.QueryEscape(runID))
	if err != nil {
		return stateBriefOutput{Note: "state brief unavailable: " + err.Error()}
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		// The dark flag and an unknown run are indistinguishable by design
		// (the surface 404s while lema-state-brief is off) — say both.
		return stateBriefOutput{
			Note: "state brief unavailable: the server has no brief for this run (the surface may not be enabled yet, or the run is unknown)",
		}
	default:
		return stateBriefOutput{Note: fmt.Sprintf("state brief unavailable: HTTP %d", status)}
	}
	var wire struct {
		Scope    string          `json:"scope"`
		Sections json.RawMessage `json:"sections"`
		Silences json.RawMessage `json:"silences"`
		AsOf     string          `json:"as_of"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return stateBriefOutput{Note: "state brief unavailable: unreadable server response"}
	}
	out := stateBriefOutput{Scope: wire.Scope, AsOf: wire.AsOf, Note: note}
	// The sub-messages are valid JSON (the outer unmarshal succeeded), so these
	// decodes cannot fail; a nil/absent field stays nil and is omitted.
	if len(wire.Sections) > 0 {
		_ = json.Unmarshal(wire.Sections, &out.Sections)
	}
	if len(wire.Silences) > 0 {
		_ = json.Unmarshal(wire.Silences, &out.Silences)
	}
	logUsage(caller, note, 1, out)
	return out
}

func getStateBrief(ctx context.Context, req *mcp.CallToolRequest, in stateBriefInput) (*mcp.CallToolResult, stateBriefOutput, error) {
	return nil, stateBrief(ctx, in.Run, "get_state_brief"), nil
}
