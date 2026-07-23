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
// Target resolution uses the process-hosted runtime and the same immutable,
// validated receipt as every other hosted operation. A legacy workspace value
// remains an explicit override; without one, verified Git or a validated local
// association can resolve the Project. Resolution failure never becomes an
// unscoped brief request.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
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

// stateBriefOutputSchema keeps stateBriefOutput as the single source of the
// tool contract while adapting jsonschema-go's permissive boolean property
// schemas to the object form required by stricter MCP clients. Only `true`
// values directly under properties are rewritten; constraints such as
// additionalProperties:false remain unchanged.
func stateBriefOutputSchema() map[string]any {
	inferred, err := jsonschema.For[stateBriefOutput](nil)
	if err != nil {
		panic(fmt.Sprintf("infer get_state_brief output schema: %v", err))
	}
	raw, err := json.Marshal(inferred)
	if err != nil {
		panic(fmt.Sprintf("marshal get_state_brief output schema: %v", err))
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		panic(fmt.Sprintf("decode get_state_brief output schema: %v", err))
	}
	properties, _ := schema["properties"].(map[string]any)
	for name, property := range properties {
		if permissive, ok := property.(bool); ok && permissive {
			properties[name] = map[string]any{}
		}
	}
	return schema
}

var getStateBriefTool = &mcp.Tool{
	Name: "get_state_brief",
	Description: "Returns the scoped State Brief for a run: objective, last checkpoint, files in flight, " +
		"settled decisions in scope (cited), binding rejected approaches, related active runs — " +
		"composed deterministically from the recorded state, with every unavailable section named " +
		"in silences. Omitting run resolves this project's prior session (the relay read).",
	Annotations:  readOnlyExternal("Get the State Brief (hosted)"),
	OutputSchema: stateBriefOutputSchema(),
}

var errNoPriorStateBriefRun = errors.New("no prior run known for this project")

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
func resolvePriorRun(ctx context.Context, s *collectorSyncer, receipt targetContext) (runID, note string, err error) {
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
		return "", "", errNoPriorStateBriefRun
	}
	harness := cp.Harness
	if harness == "" {
		// Checkpoints written before the harness field existed: the collector
		// shipped with only this adapter, so the key is stable.
		harness = "claude-code"
	}
	hosted, err := s.ensureRunInWorkspace(ctx, receipt.ProjectWorkspaceID, harness, cp.RunID, cp.CWD)
	if err != nil {
		return "", "", err
	}
	return hosted.ID, fmt.Sprintf("resolved from this project's prior session %s (%s)", cp.RunID, harness), nil
}

// stateBrief is the ONE code path the State Brief serves from — workspace
// resolution, the F4 prior-run relay read when run is empty, the GET /brief
// fetch, and the verbatim pass-through. Extracted from the tool handler so
// the lema://brief resource (brief_resource.go, decision fa8a63f4) can wrap
// it thinly with zero drift. caller labels the usage metric with which
// surface served. Every can't-serve path is an honest note in the output,
// never an error — a fresh session should read state, not a failure.
func stateBrief(ctx context.Context, run, caller string) stateBriefOutput {
	runtime, err := currentHostedRuntime()
	if err != nil {
		return stateBriefOutput{
			Note: "state brief unavailable: hosted mode is not configured (LEMA_API_URL / LEMA_API_TOKEN)",
		}
	}
	return stateBriefWithRuntime(ctx, runtime, run, caller)
}

// stateBriefWithRuntime is the operation seam: one process-owned provider
// resolves one immutable receipt before prior-Run resolution or any operation
// HTTP. Both Run identity and /brief use the receipt's Project workspace; the
// primary repository remains in the receipt and in the redacted diagnostic.
func stateBriefWithRuntime(ctx context.Context, runtime hostedWriteRuntime, run, caller string) stateBriefOutput {
	out, err := withResolvedTarget(ctx, runtime.targets, runtime.targetInput, func(ctx context.Context, receipt targetContext) (stateBriefOutput, error) {
		s := &collectorSyncer{
			apiURL: runtime.apiURL, token: runtime.token,
			workspaceID: receipt.ProjectWorkspaceID, client: runtime.client,
		}
		return stateBriefForReceipt(ctx, s, receipt, run, caller), nil
	})
	if err != nil {
		return stateBriefOutput{Note: "state brief unavailable: " + err.Error()}
	}
	return out
}

func stateBriefForReceipt(ctx context.Context, s *collectorSyncer, receipt targetContext, run, caller string) stateBriefOutput {
	runID := strings.TrimSpace(run)
	note := "explicit run id"
	if runID == "" {
		var err error
		runID, note, err = resolvePriorRun(ctx, s, receipt)
		if err != nil {
			failure := "prior run could not be resolved"
			if errors.Is(err, errNoPriorStateBriefRun) {
				failure = errNoPriorStateBriefRun.Error()
			}
			return stateBriefOutput{Note: stateBriefReceiptNote("state brief unavailable: "+failure, receipt)}
		}
	}
	status, body, err := s.get(ctx, "/brief?run="+url.QueryEscape(runID))
	if err != nil {
		return stateBriefOutput{Note: stateBriefReceiptNote("state brief unavailable: hosted request failed", receipt)}
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		// The dark flag and an unknown run are indistinguishable by design
		// (the surface 404s while lema-state-brief is off) — say both.
		return stateBriefOutput{
			Note: stateBriefReceiptNote("state brief unavailable: the server has no brief for this run (the surface may not be enabled yet, or the run is unknown)", receipt),
		}
	default:
		return stateBriefOutput{Note: stateBriefReceiptNote(fmt.Sprintf("state brief unavailable: HTTP %d", status), receipt)}
	}
	var wire struct {
		Scope    string          `json:"scope"`
		Sections json.RawMessage `json:"sections"`
		Silences json.RawMessage `json:"silences"`
		AsOf     string          `json:"as_of"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return stateBriefOutput{Note: stateBriefReceiptNote("state brief unavailable: unreadable server response", receipt)}
	}
	out := stateBriefOutput{Scope: wire.Scope, AsOf: wire.AsOf, Note: stateBriefReceiptNote(note, receipt)}
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

// stateBriefReceiptNote carries the receipt's primary repository provenance
// without exposing full workspace ids, tokens, endpoints, or local paths.
func stateBriefReceiptNote(note string, receipt targetContext) string {
	return fmt.Sprintf(
		"%s; target resolved by %s; project UUID ending %s; primary repository %s; repository UUID ending %s",
		note, receipt.ResolvedBy, redactedUUIDSuffix(receipt.ProjectWorkspaceID),
		receipt.Repository.Canonical, redactedUUIDSuffix(receipt.RepositoryWorkspaceID),
	)
}

func getStateBrief(ctx context.Context, req *mcp.CallToolRequest, in stateBriefInput) (*mcp.CallToolResult, stateBriefOutput, error) {
	return nil, stateBrief(ctx, in.Run, "get_state_brief"), nil
}
