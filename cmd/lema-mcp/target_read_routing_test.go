package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

const hiddenReadWorkspaceID = "hidden-leaf-secret"

func projectReadContext() targetContext {
	receipt := validRoutingContext()
	receipt.RepositoryWorkspaceID = "repo-primary"
	receipt.VisibleRepositoryWorkspaceIDs = []string{"repo-z", "repo-primary", "repo-z", "repo-a"}
	return receipt
}

func testHostedReadRuntime(provider targetProvider, server *httptest.Server) hostedWriteRuntime {
	runtime := testHostedWriteRuntime(provider, server)
	runtime.hosted = source.NewHosted(server.URL, runtime.token, server.Client())
	return runtime
}

func installHostedReadRuntime(t *testing.T, runtime hostedWriteRuntime) {
	t.Helper()
	oldRuntime, oldProvider := processHostedRuntime, processTargetProvider
	oldHosted, oldSrc, oldCapture := hostedSrc, src, capture
	oldUsage := usageLog
	t.Cleanup(func() {
		processHostedRuntime, processTargetProvider = oldRuntime, oldProvider
		hostedSrc, src, capture = oldHosted, oldSrc, oldCapture
		usageLog = oldUsage
	})
	processHostedRuntime = &runtime
	processTargetProvider = runtime.targets
	hostedSrc = runtime.hosted
	src = runtime.hosted
	store, err := source.NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	capture = store
}

func TestTargetRoutingReadScopeDefaultsPrimaryFirstAndNarrowsWithinReceipt(t *testing.T) {
	receipt := projectReadContext()
	got, err := hostedReadWorkspaceScope(receipt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "repo-primary,repo-z,repo-a"; strings.Join(got, ",") != want {
		t.Fatalf("default scope = %v, want stable primary-first %s", got, want)
	}

	got, err = hostedReadWorkspaceScope(receipt, []string{"repo-a", "repo-primary", "repo-a"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "repo-primary,repo-a"; strings.Join(got, ",") != want {
		t.Fatalf("narrowed scope = %v, want receipt-ordered %s", got, want)
	}

	_, err = hostedReadWorkspaceScope(receipt, []string{"repo-primary", hiddenReadWorkspaceID})
	if !errors.Is(err, errHostedReadScopeOutsideReceipt) {
		t.Fatalf("outside scope error = %v, want the redacted sentinel", err)
	}
	if strings.Contains(err.Error(), hiddenReadWorkspaceID) {
		t.Fatalf("scope error leaked hidden workspace id: %q", err)
	}
}

type readWireCapture struct {
	askScopes           [][]string
	retrieveScopes      [][]string
	checkScopes         [][]string
	checkApproachScopes [][]string
	knowledgePaths      []string
	requests            int
}

func readRoutingServer(t *testing.T, capture *readWireCapture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capture.requests++
		switch r.URL.Path {
		case "/ask":
			var body struct {
				WorkspaceIDs []string `json:"workspace_ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			capture.askScopes = append(capture.askScopes, body.WorkspaceIDs)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"scope": "visible project repositories", "answer": "primary answer [1]",
				"sources": []map[string]any{{"n": 1, "ref": "ADR-1", "type": "chosen", "text": "primary answer"}},
			})
		case "/retrieve":
			var body struct {
				WorkspaceIDs []string `json:"workspace_ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			capture.retrieveScopes = append(capture.retrieveScopes, body.WorkspaceIDs)
			_ = json.NewEncoder(w).Encode(map[string]any{"scope": "visible project repositories", "atoms": []any{}})
		case "/closed-atoms":
			capture.checkScopes = append(capture.checkScopes, r.URL.Query()["workspace_ids"])
			_ = json.NewEncoder(w).Encode(map[string]any{"atoms": []any{}})
		case "/check-approach":
			var body struct {
				WorkspaceIDs []string `json:"workspace_ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			capture.checkApproachScopes = append(capture.checkApproachScopes, body.WorkspaceIDs)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"repo": "visible project repositories", "approach": "use queues", "verdict": "no_recorded_ruling", "sources": []any{},
			})
		case "/workspaces/repo-primary/knowledge-audit":
			capture.knowledgePaths = append(capture.knowledgePaths, r.URL.Path)
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestTargetRoutingEveryHostedReadDefaultsToReceiptVisibleRepositories(t *testing.T) {
	var wire readWireCapture
	server := readRoutingServer(t, &wire)
	defer server.Close()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: projectReadContext()}}
	runtime := testHostedReadRuntime(provider, server)
	installHostedReadRuntime(t, runtime)

	if _, _, err := askHosted(context.Background(), nil, askInput{Query: "why"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolve(context.Background(), nil, resolveInput{Intent: "why", Query: "why"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolve(context.Background(), nil, resolveInput{Intent: "id", Query: "find"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkDecided(context.Background(), nil, checkInput{Topic: "use queues"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkApproach(context.Background(), nil, checkApproachInput{Approach: "use queues"}); err != nil {
		t.Fatal(err)
	}
	if out := frontloadWithRuntime(context.Background(), runtime, frontloadInput{Prompt: "why"}); out == "" {
		t.Fatal("frontload default scope returned no cited context")
	}
	cachePath := filepath.Join(t.TempDir(), ".lema", "decisions.jsonl")
	runGuardRefresh(cachePath, runtime)

	want := "repo-primary,repo-z,repo-a"
	for name, scopes := range map[string][][]string{
		"ask": wire.askScopes, "retrieve": wire.retrieveScopes,
		"check": wire.checkScopes, "check_approach": wire.checkApproachScopes,
	} {
		if len(scopes) == 0 {
			t.Fatalf("%s sent no hosted request", name)
		}
		for _, scope := range scopes {
			if got := strings.Join(scope, ","); got != want {
				t.Errorf("%s scope = %q, want %q", name, got, want)
			}
		}
	}
	if len(wire.knowledgePaths) != 1 {
		t.Fatalf("frontload knowledge requests = %v, want the primary leaf once", wire.knowledgePaths)
	}
}

func TestTargetRoutingCallerScopeOnlyNarrowsAndRejectsOutsideBeforeHostedRead(t *testing.T) {
	var wire readWireCapture
	server := readRoutingServer(t, &wire)
	defer server.Close()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: projectReadContext()}}
	runtime := testHostedReadRuntime(provider, server)
	installHostedReadRuntime(t, runtime)

	requested := []string{"repo-a", "repo-primary", "repo-a"}
	if _, _, err := askHosted(context.Background(), nil, askInput{Query: "why", WorkspaceIDs: requested}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolve(context.Background(), nil, resolveInput{Intent: "id", Query: "find", WorkspaceIDs: requested}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkDecided(context.Background(), nil, checkInput{Topic: "use queues", WorkspaceIDs: requested}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkApproach(context.Background(), nil, checkApproachInput{Approach: "use queues", WorkspaceIDs: requested}); err != nil {
		t.Fatal(err)
	}
	for name, scopes := range map[string][][]string{
		"ask": wire.askScopes, "retrieve": wire.retrieveScopes,
		"check": wire.checkScopes, "check_approach": wire.checkApproachScopes,
	} {
		if len(scopes) != 1 || strings.Join(scopes[0], ",") != "repo-primary,repo-a" {
			t.Errorf("%s narrowed scopes = %v, want [repo-primary repo-a]", name, scopes)
		}
	}

	bad := []string{"repo-primary", hiddenReadWorkspaceID}
	before := wire.requests
	var errs []error
	_, _, err := askHosted(context.Background(), nil, askInput{Query: "why", WorkspaceIDs: bad})
	errs = append(errs, err)
	_, _, err = resolve(context.Background(), nil, resolveInput{Intent: "id", Query: "find", WorkspaceIDs: bad})
	errs = append(errs, err)
	_, _, err = checkDecided(context.Background(), nil, checkInput{Topic: "use queues", WorkspaceIDs: bad})
	errs = append(errs, err)
	_, _, err = checkApproach(context.Background(), nil, checkApproachInput{Approach: "use queues", WorkspaceIDs: bad})
	errs = append(errs, err)
	if wire.requests != before {
		t.Fatalf("outside scope sent %d hosted read request(s), want zero", wire.requests-before)
	}
	for _, err := range errs {
		if !errors.Is(err, errHostedReadScopeOutsideReceipt) {
			t.Errorf("outside scope error = %v, want redacted sentinel", err)
		}
		if err != nil && strings.Contains(err.Error(), hiddenReadWorkspaceID) {
			t.Errorf("outside scope error leaked hidden leaf: %q", err)
		}
	}
}

func TestTargetRoutingEveryNonResolvedStatusSendsNoHostedRead(t *testing.T) {
	statuses := []resolutionStatus{resolutionUnresolved, resolutionAmbiguous, resolutionForbidden, resolutionStale}
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			var wire readWireCapture
			server := readRoutingServer(t, &wire)
			defer server.Close()
			provider := &fakeTargetProvider{result: resolutionResult{Status: status, Reason: hiddenReadWorkspaceID}}
			runtime := testHostedReadRuntime(provider, server)
			installHostedReadRuntime(t, runtime)

			_, _, _ = askHosted(context.Background(), nil, askInput{Query: "why"})
			_, _, _ = resolve(context.Background(), nil, resolveInput{Intent: "why", Query: "why"})
			_, _, _ = resolve(context.Background(), nil, resolveInput{Intent: "id", Query: "find"})
			_, _, _ = checkDecided(context.Background(), nil, checkInput{Topic: "use queues"})
			_, _, _ = checkApproach(context.Background(), nil, checkApproachInput{Approach: "use queues"})
			if out := frontloadWithRuntime(context.Background(), runtime, frontloadInput{Prompt: "why"}); out != "" {
				t.Errorf("frontload on %s context emitted %q", status, out)
			}
			cachePath := filepath.Join(t.TempDir(), ".lema", "decisions.jsonl")
			runGuardRefresh(cachePath, runtime)
			if wire.requests != 0 {
				t.Fatalf("%s context sent %d hosted read request(s), want zero", status, wire.requests)
			}
			if _, err := os.Stat(guardCacheFile(cachePath)); !os.IsNotExist(err) {
				t.Fatalf("%s context replaced guard cache, stat err=%v", status, err)
			}
		})
	}
}

func TestGuardRefreshUnresolvedKeepsLocalEnforcementAndEmitsOnlyRedactedCounter(t *testing.T) {
	var wire readWireCapture
	server := readRoutingServer(t, &wire)
	defer server.Close()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionUnresolved, Reason: hiddenReadWorkspaceID}}
	runtime := testHostedReadRuntime(provider, server)
	cachePath := filepath.Join(t.TempDir(), ".lema", "decisions.jsonl")

	guardRefreshUnresolvedTotal.Store(0)
	diagnostic := captureReadRoutingStderr(t, func() { runGuardRefresh(cachePath, runtime) })
	if wire.requests != 0 {
		t.Fatalf("unresolved guard refresh sent %d hosted reads", wire.requests)
	}
	if got := guardRefreshUnresolvedTotal.Load(); got != 1 {
		t.Fatalf("guard unresolved counter = %d, want 1", got)
	}
	if !strings.Contains(diagnostic, "guard_refresh_target_unresolved_total") || !strings.Contains(diagnostic, `"value":1`) {
		t.Fatalf("guard diagnostic missing redacted counter: %q", diagnostic)
	}
	for _, forbidden := range []string{hiddenReadWorkspaceID, runtime.apiURL, runtime.token, runtime.targetInput.CWD} {
		if strings.Contains(diagnostic, forbidden) {
			t.Fatalf("guard diagnostic leaked %q: %q", forbidden, diagnostic)
		}
	}

	closed := []source.Atom{{
		ID: "local-closure", Ref: "ADR-1", Type: "rejected_alternative", Text: "Kafka rejected",
		Closed: true, ClosedNote: "do not propose Kafka", MatchKey: "Kafka",
	}}
	out, atom := evaluateGuard(closed, ctxQuery("queue.go", "kafka.NewProducer()"), guardModeContext)
	if out == nil || atom == nil || atom.Ref != "ADR-1" {
		t.Fatalf("local-only guard enforcement disappeared after unresolved refresh: out=%v atom=%+v", out, atom)
	}
}

func TestTargetRoutingHiddenLeafAbsentFromRequestOutputErrorAndUsageLog(t *testing.T) {
	var wire readWireCapture
	server := readRoutingServer(t, &wire)
	defer server.Close()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: projectReadContext()}}
	runtime := testHostedReadRuntime(provider, server)
	installHostedReadRuntime(t, runtime)

	logFile, err := os.CreateTemp(t.TempDir(), "usage-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	usageLog = logFile
	_, out, err := askHosted(context.Background(), nil, askInput{Query: "why"})
	if err != nil {
		t.Fatal(err)
	}
	bad := []string{"repo-primary", hiddenReadWorkspaceID}
	_, _, scopeErr := resolve(context.Background(), nil, resolveInput{Intent: "why", Query: "why", WorkspaceIDs: bad})
	if scopeErr == nil {
		t.Fatal("hidden workspace was accepted as a narrowing")
	}
	if err := logFile.Sync(); err != nil {
		t.Fatal(err)
	}
	logBytes, err := os.ReadFile(logFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	outBytes, _ := json.Marshal(out)
	wireBytes, _ := json.Marshal(wire)
	for label, data := range map[string][]byte{
		"request": wireBytes, "output": outBytes, "error": []byte(scopeErr.Error()), "log": logBytes,
	} {
		if strings.Contains(string(data), hiddenReadWorkspaceID) {
			t.Fatalf("hidden leaf appeared in %s: %s", label, data)
		}
	}
}

func captureReadRoutingStderr(t *testing.T, run func()) string {
	t.Helper()
	old := os.Stderr
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = write
	run()
	_ = write.Close()
	os.Stderr = old
	data, err := io.ReadAll(read)
	if err != nil {
		_ = read.Close()
		t.Fatal(err)
	}
	_ = read.Close()
	return string(data)
}
