package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
)

func testHostedWriteRuntime(provider targetProvider, server *httptest.Server) hostedWriteRuntime {
	return hostedWriteRuntime{
		client:      server.Client(),
		apiURL:      server.URL,
		token:       "lema_live_routing_secret",
		targets:     provider,
		targetInput: resolveTargetInput{CWD: "/private/operator/repository"},
		now:         func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) },
	}
}

type orderedTargetProvider struct {
	result resolutionResult
	events *[]string
	calls  int
}

func (p *orderedTargetProvider) Resolve(context.Context, resolveTargetInput) (resolutionResult, error) {
	p.calls++
	*p.events = append(*p.events, "resolve")
	return p.result, nil
}

func leafRoutingContext() targetContext {
	receipt := validRoutingContext()
	receipt.ProjectWorkspaceID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	receipt.RepositoryWorkspaceID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	receipt.VisibleRepositoryWorkspaceIDs = []string{receipt.RepositoryWorkspaceID}
	return receipt
}

func TestTargetRoutingRecordAndProposeUseReceiptRepositoryLeaf(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		_ = json.NewEncoder(w).Encode(pushResponse{Created: 1, Results: []pushResult{{Status: "created", CurrentStatus: "proposed"}}})
	}))
	defer server.Close()

	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: leafRoutingContext()}}
	recorder := newHostedRecorder(testHostedWriteRuntime(provider, server), nil, "")
	previous := decisionRecorder
	decisionRecorder = recorder
	t.Cleanup(func() { decisionRecorder = previous })

	if _, _, err := recordDecision(context.Background(), nil, recordInput{Title: "record route", Chosen: "leaf"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := propose(context.Background(), nil, proposeInput{Title: "propose route", Chosen: "leaf"}); err != nil {
		t.Fatal(err)
	}

	want := "/workspaces/" + leafRoutingContext().RepositoryWorkspaceID + "/import-decisions"
	if len(paths) != 2 || paths[0] != want || paths[1] != want {
		t.Fatalf("record/propose paths = %v, want [%s %s]", paths, want, want)
	}
	if provider.calls != 2 {
		t.Fatalf("target resolutions = %d, want one immutable receipt per operation", provider.calls)
	}
}

func TestTargetRoutingPushHookResolvesBeforeGateScanAndRepositoryLeafUpload(t *testing.T) {
	var events []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		events = append(events, request.URL.Path)
		switch request.URL.Path {
		case "/push-enabled":
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": true})
		case "/workspaces/" + leafRoutingContext().RepositoryWorkspaceID + "/import-decisions":
			_ = json.NewEncoder(w).Encode(pushResponse{Created: 1})
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	provider := &orderedTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: leafRoutingContext()}, events: &events}

	n := runPushInput(context.Background(), testHostedWriteRuntime(provider, server), stopHookInput{TranscriptPath: "not-opened-by-test"}, func(path string) ([]pushCandidate, error) {
		events = append(events, "scan:"+path)
		return []pushCandidate{{Approach: "Use the repository leaf"}}, nil
	})
	if n != 1 || provider.calls != 1 {
		t.Fatalf("pushed=%d provider calls=%d, want 1/1", n, provider.calls)
	}
	want := []string{"resolve", "/push-enabled", "scan:not-opened-by-test", "/workspaces/" + leafRoutingContext().RepositoryWorkspaceID + "/import-decisions"}
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Fatalf("push hook events = %v, want %v", events, want)
	}
}

func TestTargetRoutingDistillHookResolvesBeforeGateScanAndRepositoryLeafUpload(t *testing.T) {
	var events []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		events = append(events, request.URL.Path)
		switch request.URL.Path {
		case "/session-distill-enabled":
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": true})
		case "/workspaces/" + leafRoutingContext().RepositoryWorkspaceID + "/ingest-session":
			_ = json.NewEncoder(w).Encode(distillResponse{SessionID: "session-1", Status: "ingested", Claims: 1})
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	provider := &orderedTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: leafRoutingContext()}, events: &events}

	n := runDistillInput(context.Background(), testHostedWriteRuntime(provider, server), stopHookInput{SessionID: "session-1", TranscriptPath: "not-opened-by-test"}, func(path string) (distilled, error) {
		events = append(events, "scan:"+path)
		return distilled{Text: "User: choose leaf"}, nil
	})
	if n != 1 || provider.calls != 1 {
		t.Fatalf("harvested=%d provider calls=%d, want 1/1", n, provider.calls)
	}
	want := []string{"resolve", "/session-distill-enabled", "scan:not-opened-by-test", "/workspaces/" + leafRoutingContext().RepositoryWorkspaceID + "/ingest-session"}
	if strings.Join(events, "|") != strings.Join(want, "|") {
		t.Fatalf("distill hook events = %v, want %v", events, want)
	}
}

func TestTargetRoutingSettleValidatesDecisionInReceiptRepositoryLeaf(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.RequestURI())
		switch {
		case strings.Contains(request.URL.Path, "/workspaces/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"decisions": []settleDecision{{ID: settleTestID, Title: "leaf decision", CurrentStatus: "proposed"}}})
		case strings.HasSuffix(request.URL.Path, "/events"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": settleTestID})
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: leafRoutingContext()}}

	if err := settleWithTarget(context.Background(), testHostedWriteRuntime(provider, server), []string{"reject", settleTestID, "--reason", "superseded"}); err != nil {
		t.Fatal(err)
	}
	wantPrefix := "/workspaces/" + leafRoutingContext().RepositoryWorkspaceID + "/decisions"
	if len(paths) != 2 || !strings.HasPrefix(paths[0], wantPrefix) || paths[1] != "/decisions/"+settleTestID+"/events" {
		t.Fatalf("settle paths = %v, want leaf validation then decision event", paths)
	}
}

func TestTargetRoutingEveryUnresolvedInteractiveOperationSendsNoHTTP(t *testing.T) {
	operations := []struct {
		name string
		run  func(context.Context, hostedWriteRuntime) error
	}{
		{"record", func(ctx context.Context, runtime hostedWriteRuntime) error {
			_, err := newHostedRecorder(runtime, nil, "").record(ctx, source.DecisionRecord{Title: "record", Chosen: "leaf"})
			return err
		}},
		{"propose", func(ctx context.Context, runtime hostedWriteRuntime) error {
			previous := decisionRecorder
			decisionRecorder = newHostedRecorder(runtime, nil, "")
			defer func() { decisionRecorder = previous }()
			_, _, err := propose(ctx, nil, proposeInput{Title: "propose", Chosen: "leaf"})
			return err
		}},
		{"settle", func(ctx context.Context, runtime hostedWriteRuntime) error {
			return settleWithTarget(ctx, runtime, []string{"reject", settleTestID, "--reason", "x"})
		}},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
			defer server.Close()
			provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionUnresolved}}
			_ = operation.run(context.Background(), testHostedWriteRuntime(provider, server))
			if provider.calls != 1 || requests != 0 {
				t.Fatalf("provider calls=%d HTTP requests=%d, want 1/0", provider.calls, requests)
			}
		})
	}
}

func TestTargetRoutingNonResolvedHooksSendNoGateOrUploadHTTPAndDoNotScan(t *testing.T) {
	results := []struct {
		name   string
		result resolutionResult
	}{
		{"unresolved", resolutionResult{Status: resolutionUnresolved}},
		{"ambiguous", resolutionResult{Status: resolutionAmbiguous}},
		{"forbidden", resolutionResult{Status: resolutionForbidden}},
		{"stale", resolutionResult{Status: resolutionStale}},
		{"malformed", resolutionResult{Status: resolutionResolved, Context: targetContext{}}},
	}
	for _, hook := range []string{"push", "distill"} {
		for _, tc := range results {
			t.Run(hook+"/"+tc.name, func(t *testing.T) {
				requests, scans := 0, 0
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
				defer server.Close()
				provider := &fakeTargetProvider{result: tc.result}
				runtime := testHostedWriteRuntime(provider, server)
				switch hook {
				case "push":
					runPushInput(context.Background(), runtime, stopHookInput{TranscriptPath: "must-not-open"}, func(string) ([]pushCandidate, error) {
						scans++
						return []pushCandidate{{Approach: "must not route"}}, nil
					})
				case "distill":
					runDistillInput(context.Background(), runtime, stopHookInput{SessionID: "session-1", TranscriptPath: "must-not-open"}, func(string) (distilled, error) {
						scans++
						return distilled{Text: "must not route"}, nil
					})
				}
				if provider.calls != 1 || requests != 0 || scans != 0 {
					t.Fatalf("provider calls=%d HTTP requests=%d scans=%d, want 1/0/0", provider.calls, requests, scans)
				}
			})
		}
	}
}

func TestTargetRoutingOfflineDraftPersistsRedactedReceiptAndRevalidatesBeforeRetryUpload(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "decisions.jsonl")
	store, err := source.NewCaptureStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	receipt := leafRoutingContext()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: receipt}}
	runtime := testHostedWriteRuntime(provider, server)
	recorder := newHostedRecorder(runtime, store, storePath)
	record := source.DecisionRecord{Title: "offline route", Chosen: "repository leaf"}

	out, err := recorder.record(context.Background(), record)
	if err != nil || out.Status != "local_draft" {
		t.Fatalf("offline fallback = (%+v, %v), want local draft", out, err)
	}
	data, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{runtime.token, runtime.apiURL, credentialFingerprint(runtime.token), "operator", runtime.targetInput.CWD} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("persisted resolver evidence leaked %q: %s", forbidden, data)
		}
	}
	for _, required := range []string{receipt.ProjectWorkspaceID, receipt.RepositoryWorkspaceID, receipt.Repository.Canonical, receipt.ResolvedBy} {
		if !strings.Contains(string(data), required) {
			t.Fatalf("persisted resolver evidence missing %q: %s", required, data)
		}
	}

	staleProvider := &fakeTargetProvider{result: resolutionResult{Status: resolutionStale}}
	staleRuntime := testHostedWriteRuntime(staleProvider, server)
	staleRecorder := newHostedRecorder(staleRuntime, store, storePath)
	requestsBeforeRetry := requests
	_, err = staleRecorder.record(context.Background(), record)
	if targetResolutionStatusFromError(err) != resolutionStale {
		t.Fatalf("stale retry error = %v, want typed stale", err)
	}
	if requests != requestsBeforeRetry {
		t.Fatalf("stale retry sent HTTP: before=%d after=%d", requestsBeforeRetry, requests)
	}
	if staleProvider.input.LocalAssociation == nil || staleProvider.input.LocalAssociation.ProjectWorkspaceID != receipt.ProjectWorkspaceID || staleProvider.input.LocalAssociation.RepositoryWorkspaceID != receipt.RepositoryWorkspaceID {
		t.Fatalf("retry did not revalidate the persisted receipt: %+v", staleProvider.input)
	}
}

func TestTargetRoutingTruncatedIDCollisionCannotReuseOfflineReceipt(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "decisions.jsonl")
	store, err := source.NewCaptureStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	receipt := leafRoutingContext()
	if _, err := store.RecordDraft(source.DecisionRecord{
		Title:          "title-1821",
		Chosen:         "chosen-1821",
		TargetEvidence: targetEvidenceFromContext(receipt),
	}); err != nil {
		t.Fatal(err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionUnresolved}}
	recorder := newHostedRecorder(testHostedWriteRuntime(provider, server), store, storePath)
	out, err := recorder.record(context.Background(), source.DecisionRecord{Title: "title-4163", Chosen: "chosen-4163"})
	if err != nil || out.Status != "local_draft" {
		t.Fatalf("colliding capture fallback = (%+v, %v), want local draft", out, err)
	}
	if provider.input.LocalAssociation != nil {
		t.Fatalf("colliding capture reused receipt A: %+v", provider.input.LocalAssociation)
	}
	if requests != 0 {
		t.Fatalf("colliding capture sent %d operation requests, want zero", requests)
	}
}
