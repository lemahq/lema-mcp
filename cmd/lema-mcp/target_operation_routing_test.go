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

func TestTargetRoutingPushUsesReceiptRepositoryLeaf(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		path = request.URL.Path
		_ = json.NewEncoder(w).Encode(pushResponse{Created: 1})
	}))
	defer server.Close()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: leafRoutingContext()}}

	_, err := pushWithTarget(context.Background(), testHostedWriteRuntime(provider, server), []pushRecord{{ID: "d_1", Title: "push", Chosen: "leaf", Status: pushStatusProposed}})
	if err != nil {
		t.Fatal(err)
	}
	want := "/workspaces/" + leafRoutingContext().RepositoryWorkspaceID + "/import-decisions"
	if path != want {
		t.Fatalf("push path = %q, want %q", path, want)
	}
}

func TestTargetRoutingDistillUsesReceiptRepositoryLeaf(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		path = request.URL.Path
		_ = json.NewEncoder(w).Encode(distillResponse{SessionID: "session-1", Status: "ingested", Claims: 1})
	}))
	defer server.Close()
	provider := &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: leafRoutingContext()}}

	_, err := distillWithTarget(context.Background(), testHostedWriteRuntime(provider, server), "session-1", distilled{Text: "User: choose leaf"})
	if err != nil {
		t.Fatal(err)
	}
	want := "/workspaces/" + leafRoutingContext().RepositoryWorkspaceID + "/ingest-session"
	if path != want {
		t.Fatalf("distill path = %q, want %q", path, want)
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

func TestTargetRoutingEveryUnresolvedOperationSendsNoHTTP(t *testing.T) {
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
		{"push", func(ctx context.Context, runtime hostedWriteRuntime) error {
			_, err := pushWithTarget(ctx, runtime, []pushRecord{{ID: "d_1", Title: "push", Chosen: "leaf"}})
			return err
		}},
		{"distill", func(ctx context.Context, runtime hostedWriteRuntime) error {
			_, err := distillWithTarget(ctx, runtime, "session-1", distilled{Text: "User: leaf"})
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
