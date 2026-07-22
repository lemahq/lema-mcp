package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	briefProjectID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	briefRepoID    = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	briefRunID     = "22222222-2222-2222-2222-222222222222"
)

// get_state_brief's Target Context contract: resolve one receipt before any
// run or brief operation; explicit target evidence wins without a Git fallback;
// otherwise verified Git may resolve the Project; every non-resolved or
// malformed receipt stops before operation HTTP. Runs and /brief are both
// Project-homed while the receipt keeps the primary repository provenance.

type stateBriefCapture struct {
	requests   int
	paths      []string
	runCreates int
	runCreate  map[string]string
	briefRuns  []string
}

func stateBriefRoutingContext() targetContext {
	receipt := validRoutingContext()
	receipt.ProjectWorkspaceID = briefProjectID
	receipt.RepositoryWorkspaceID = briefRepoID
	receipt.VisibleRepositoryWorkspaceIDs = []string{briefRepoID}
	receipt.Repository = repositoryIdentity{
		Host: "github.com", Owner: "acme", Name: "payments-api",
		Canonical: "git:github.com/acme/payments-api",
	}
	return receipt
}

func newBriefTestServer(t *testing.T, wantHarness string, briefStatus int) (*httptest.Server, *stateBriefCapture) {
	t.Helper()
	return newBriefTestServerForProject(t, briefProjectID, wantHarness, briefStatus)
}

func newBriefTestServerForProject(t *testing.T, projectID, wantHarness string, briefStatus int) (*httptest.Server, *stateBriefCapture) {
	t.Helper()
	cap := &stateBriefCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.requests++
		cap.paths = append(cap.paths, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer lema_live_routing_secret" {
			t.Errorf("authorization = %q, want bearer token", got)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/workspaces/"+projectID+"/runs":
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req["harness"] != wantHarness {
				t.Errorf("harness = %q, want %q (a drifted key mints a second identity)", req["harness"], wantHarness)
			}
			cap.runCreates++
			cap.runCreate = req
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"run":{"id":"` + briefRunID + `"},"created":false,"rung":7}`))
		case r.Method == http.MethodGet && r.URL.Path == "/workspaces/"+projectID+"/brief":
			cap.briefRuns = append(cap.briefRuns, r.URL.Query().Get("run"))
			w.WriteHeader(briefStatus)
			_, _ = w.Write([]byte(`{"scope":"work unit wu-1","sections":[{"name":"objective","lines":[{"text":"ship it","cite":"work_unit:wu-1"}]}],"silences":["test status — not captured in v1"],"as_of":"2026-07-21T00:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return server, cap
}

func installStateBriefRuntime(t *testing.T, provider targetProvider, server *httptest.Server, input resolveTargetInput) hostedWriteRuntime {
	t.Helper()
	runtime := testHostedReadRuntime(provider, server)
	runtime.targetInput = input
	installHostedReadRuntime(t, runtime)
	return runtime
}

// TestStateBriefOverMCPSessionNonEmptyBrief drives get_state_brief through a
// real in-memory MCP client/server session — the path where the go-sdk validates
// the tool's structured output against its inferred output schema. Keep this
// regression on permissive `any`: typed client mirrors of the server-owned wire
// contract are deliberately rejected by lema:d_94d86f.
func TestStateBriefOverMCPSessionNonEmptyBrief(t *testing.T) {
	srv, _ := newBriefTestServer(t, "", http.StatusOK)
	defer srv.Close()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: stateBriefRoutingContext()}}
	installStateBriefRuntime(t, provider, srv, resolveTargetInput{CWD: "/private/operator/payments-api"})

	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "lema-mcp", Version: "test"}, nil)
	mcp.AddTool(server, getStateBriefTool, getStateBrief)
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_state_brief",
		Arguments: map[string]any{"run": briefRunID},
	})
	if err != nil {
		t.Fatalf("a non-empty brief must survive the SDK's output-schema validation: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool errored over the session: %+v", res.Content)
	}

	got, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Scope    string `json:"scope"`
		Sections []struct {
			Name  string `json:"name"`
			Lines []struct {
				Text string `json:"text"`
				Cite string `json:"cite"`
			} `json:"lines"`
		} `json:"sections"`
		Silences []string `json:"silences"`
		AsOf     string   `json:"as_of"`
	}
	if err := json.Unmarshal(got, &out); err != nil {
		t.Fatalf("structured content unreadable: %v\n%s", err, got)
	}
	if out.Scope != "work unit wu-1" || out.AsOf != "2026-07-21T00:00:00Z" {
		t.Fatalf("scope/as_of drifted: %s", got)
	}
	if len(out.Sections) != 1 || out.Sections[0].Name != "objective" ||
		len(out.Sections[0].Lines) != 1 || out.Sections[0].Lines[0].Text != "ship it" ||
		out.Sections[0].Lines[0].Cite != "work_unit:wu-1" {
		t.Fatalf("sections must pass through verbatim: %s", got)
	}
	if len(out.Silences) != 1 || out.Silences[0] != "test status — not captured in v1" {
		t.Fatalf("silences must pass through verbatim: %s", got)
	}
}

func TestStateBriefExplicitValidOverrideWinsAndRoutesProject(t *testing.T) {
	srv, cap := newBriefTestServerForProject(t, "project-payments", "", http.StatusOK)
	defer srv.Close()
	base := resolverFixture(t)
	base.parents = []string{"project-payments"}
	base.git = gitTargetEvidence{RemoteURL: "https://github.com/acme/wrong-repository.git", Root: "/private/wrong"}
	runtime := installStateBriefRuntime(t, base.resolver(), srv, resolveTargetInput{
		ExplicitWorkspaceID: "repo-api",
		CWD:                 "/private/operator/payments-api",
	})

	_, out, err := getStateBrief(context.Background(), nil, stateBriefInput{Run: briefRunID})
	if err != nil {
		t.Fatal(err)
	}
	if out.Scope != "work unit wu-1" || !strings.Contains(out.Note, "explicit run id") {
		t.Fatalf("out = %+v", out)
	}
	if base.gitCalls != 0 {
		t.Fatalf("valid explicit override consulted Git %d time(s), want zero", base.gitCalls)
	}
	if cap.runCreates != 0 || strings.Join(cap.paths, ",") != "/workspaces/project-payments/brief" {
		t.Fatalf("paths = %v, run creates=%d; want Project /brief only", cap.paths, cap.runCreates)
	}
	for _, want := range []string{"primary repository git:github.com/acme/api", "repository UUID ending [redacted]", "resolved by explicit"} {
		if !strings.Contains(out.Note, want) {
			t.Errorf("redacted receipt diagnostic missing %q: %q", want, out.Note)
		}
	}
	for _, secret := range []string{runtime.apiURL, runtime.token, runtime.targetInput.CWD, "repo-api", "project-payments"} {
		if strings.Contains(out.Note, secret) {
			t.Errorf("receipt diagnostic leaked %q: %q", secret, out.Note)
		}
	}
}

func TestStateBriefStaleExplicitOverrideSendsNoOperationAndDoesNotFallbackToGit(t *testing.T) {
	srv, cap := newBriefTestServerForProject(t, "project-payments", "", http.StatusOK)
	defer srv.Close()
	base := resolverFixture(t)
	base.parents = []string{"project-payments"}
	installStateBriefRuntime(t, base.resolver(), srv, resolveTargetInput{
		ExplicitWorkspaceID: "removed-repository",
		CWD:                 "/private/operator/payments-api",
	})

	_, out, err := getStateBrief(context.Background(), nil, stateBriefInput{Run: briefRunID})
	if err != nil {
		t.Fatal(err)
	}
	if cap.requests != 0 {
		t.Fatalf("stale explicit target sent %d operation request(s), want zero", cap.requests)
	}
	if base.gitCalls != 0 {
		t.Fatalf("stale explicit target fell back to Git %d time(s), want zero", base.gitCalls)
	}
	if out.Scope != "" || !strings.Contains(out.Note, "target lookup stale") {
		t.Fatalf("out = %+v, want redacted stale diagnostic", out)
	}
	if strings.Contains(out.Note, "removed-repository") {
		t.Fatalf("stale diagnostic leaked the explicit target: %q", out.Note)
	}
}

func TestStateBriefWithoutPinResolvesVerifiedGitAndRoutesProject(t *testing.T) {
	srv, cap := newBriefTestServerForProject(t, "project-payments", "", http.StatusOK)
	defer srv.Close()
	base := resolverFixture(t)
	base.parents = []string{"project-payments"}
	installStateBriefRuntime(t, base.resolver(), srv, resolveTargetInput{CWD: "/private/operator/payments-api"})

	_, out, err := getStateBrief(context.Background(), nil, stateBriefInput{Run: briefRunID})
	if err != nil {
		t.Fatal(err)
	}
	if base.gitCalls != 1 {
		t.Fatalf("Git resolutions = %d, want one verified resolution", base.gitCalls)
	}
	if cap.runCreates != 0 || strings.Join(cap.paths, ",") != "/workspaces/project-payments/brief" {
		t.Fatalf("paths = %v, run creates=%d; want Project /brief only", cap.paths, cap.runCreates)
	}
	if !strings.Contains(out.Note, "primary repository git:github.com/acme/api") || !strings.Contains(out.Note, "resolved by canonical_git") {
		t.Fatalf("output lost primary repository receipt provenance: %+v", out)
	}
}

func TestStateBriefResolvesPriorRunThenCreatesAndReadsUnderOneProjectReceipt(t *testing.T) {
	srv, cap := newBriefTestServer(t, "claude-code", http.StatusOK)
	defer srv.Close()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: stateBriefRoutingContext()}}
	installStateBriefRuntime(t, provider, srv, resolveTargetInput{CWD: "/private/operator/payments-api"})

	restoreRemote, restoreBranch := gitRemoteURL, gitCurrentBranch
	t.Cleanup(func() { gitRemoteURL, gitCurrentBranch = restoreRemote, restoreBranch })
	gitRemoteURL = func(string) (string, bool) { return "git@github.com:acme/payments-api.git", true }
	gitCurrentBranch = func(string) (string, bool) { return "feat/context", true }

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv(collectorDirEnv, dir)
	cp := distillEnvelopes([]collectorEnvelope{{
		RunID: "sess-prior", TS: time.Now().UTC().Format(time.RFC3339), Kind: "user_prompt",
		Payload:  map[string]string{"prompt": "resume me"},
		Evidence: map[string]string{"harness": "claude-code", "cwd": cwd},
	}}, cwd)
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}

	_, out, err := getStateBrief(context.Background(), nil, stateBriefInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Note, "sess-prior") {
		t.Fatalf("note must attribute the resolved prior run: %+v", out)
	}
	if provider.calls != 1 {
		t.Fatalf("target resolutions = %d, want one receipt before prior-Run resolution", provider.calls)
	}
	if cap.runCreates != 1 || cap.runCreate["repo"] != "acme/payments-api" || cap.runCreate["branch"] != "feat/context" {
		t.Fatalf("run create = %#v, count=%d; want primary repository provenance", cap.runCreate, cap.runCreates)
	}
	wantPaths := "/workspaces/" + briefProjectID + "/runs,/workspaces/" + briefProjectID + "/brief"
	if got := strings.Join(cap.paths, ","); got != wantPaths {
		t.Fatalf("operation paths = %q, want Project-homed %q", got, wantPaths)
	}
	if len(cap.briefRuns) != 1 || cap.briefRuns[0] != briefRunID {
		t.Fatalf("brief must be fetched for the ensured hosted run: %v", cap.briefRuns)
	}
}

func TestStateBriefEveryNonResolvedOrMalformedReceiptSendsNoOperation(t *testing.T) {
	tests := []struct {
		name   string
		result resolutionResult
	}{
		{"unresolved", resolutionResult{Status: resolutionUnresolved, Reason: hiddenReadWorkspaceID}},
		{"ambiguous", resolutionResult{Status: resolutionAmbiguous, Reason: hiddenReadWorkspaceID}},
		{"forbidden", resolutionResult{Status: resolutionForbidden, Reason: hiddenReadWorkspaceID}},
		{"stale", resolutionResult{Status: resolutionStale, Reason: hiddenReadWorkspaceID}},
		{"malformed", resolutionResult{Status: resolutionResolved, Context: targetContext{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, cap := newBriefTestServer(t, "", http.StatusOK)
			defer srv.Close()
			provider := &fakeTargetProvider{result: tc.result}
			runtime := installStateBriefRuntime(t, provider, srv, resolveTargetInput{CWD: "/private/operator/payments-api"})

			_, out, err := getStateBrief(context.Background(), nil, stateBriefInput{Run: briefRunID})
			if err != nil {
				t.Fatal(err)
			}
			if cap.requests != 0 {
				t.Fatalf("hosted operation requests = %d, want zero", cap.requests)
			}
			if out.Scope != "" || !strings.Contains(out.Note, "state brief unavailable: target lookup") {
				t.Fatalf("out = %+v, want honest redacted unavailability", out)
			}
			for _, hidden := range []string{hiddenReadWorkspaceID, tc.result.Reason, runtime.apiURL, runtime.token, runtime.targetInput.CWD} {
				if hidden != "" && strings.Contains(out.Note, hidden) {
					t.Fatalf("diagnostic leaked %q: %q", hidden, out.Note)
				}
			}
		})
	}
}

func TestStateBriefOperationFailureDiagnosticIsRedacted(t *testing.T) {
	srv, _ := newBriefTestServer(t, "", http.StatusOK)
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: stateBriefRoutingContext()}}
	runtime := installStateBriefRuntime(t, provider, srv, resolveTargetInput{CWD: "/private/operator/payments-api"})
	srv.Close()

	_, out, err := getStateBrief(context.Background(), nil, stateBriefInput{Run: briefRunID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Note, "state brief unavailable") || !strings.Contains(out.Note, "primary repository git:github.com/acme/payments-api") {
		t.Fatalf("failure lost honest state or safe receipt provenance: %+v", out)
	}
	for _, secret := range []string{runtime.apiURL, runtime.token, runtime.targetInput.CWD, briefProjectID, briefRepoID} {
		if strings.Contains(out.Note, secret) {
			t.Errorf("operation failure diagnostic leaked %q: %q", secret, out.Note)
		}
	}
}

func TestStateBriefHonestWhenUnavailable(t *testing.T) {
	// No checkpoint for this project → honest note, no fabricated scope.
	srv, _ := newBriefTestServer(t, "claude-code", http.StatusOK)
	defer srv.Close()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: stateBriefRoutingContext()}}
	installStateBriefRuntime(t, provider, srv, resolveTargetInput{CWD: "/private/operator/payments-api"})
	t.Setenv(collectorDirEnv, t.TempDir())
	_, out, err := getStateBrief(context.Background(), nil, stateBriefInput{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Scope != "" || !strings.Contains(out.Note, "no prior run known") {
		t.Fatalf("out = %+v", out)
	}
	if cwd, cwdErr := os.Getwd(); cwdErr == nil && strings.Contains(out.Note, cwd) {
		t.Fatalf("no-checkpoint diagnostic leaked cwd %q: %q", cwd, out.Note)
	}

	// Dark surface (404 while lema-state-brief is off) → honest note.
	dark, _ := newBriefTestServer(t, "", http.StatusNotFound)
	defer dark.Close()
	installStateBriefRuntime(t, provider, dark, resolveTargetInput{CWD: "/private/operator/payments-api"})
	_, out, err = getStateBrief(context.Background(), nil, stateBriefInput{Run: briefRunID})
	if err != nil {
		t.Fatal(err)
	}
	if out.Scope != "" || !strings.Contains(out.Note, "unavailable") {
		t.Fatalf("dark surface must be an honest note: %+v", out)
	}

	// No process-hosted runtime at all. A workspace pin is no longer required.
	processHostedRuntime = nil
	processTargetProvider = nil
	_, out, err = getStateBrief(context.Background(), nil, stateBriefInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Note, "hosted mode is not configured") || strings.Contains(out.Note, "LEMA_WORKSPACE_ID") {
		t.Fatalf("missing runtime must be named without requiring a workspace pin: %+v", out)
	}
}
