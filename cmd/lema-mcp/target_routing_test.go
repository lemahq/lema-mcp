package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeTargetProvider struct {
	result resolutionResult
	err    error
	calls  int
}

func (p *fakeTargetProvider) Resolve(context.Context, resolveTargetInput) (resolutionResult, error) {
	p.calls++
	return p.result, p.err
}

func TestWithResolvedTargetCallsOperationWithAnIsolatedReceipt(t *testing.T) {
	provider := &fakeTargetProvider{result: resolutionResult{
		Status: resolutionResolved,
		Context: targetContext{
			OrganizationID:                "org-1",
			ProjectWorkspaceID:            "project-1",
			RepositoryWorkspaceID:         "repository-1",
			VisibleRepositoryWorkspaceIDs: []string{"repository-1", "repository-2"},
			Evidence:                      []resolutionEvidence{{Kind: "canonical_remote", Value: "git:example.test/acme/api"}},
		},
	}}
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	got, err := withResolvedTarget(context.Background(), provider, resolveTargetInput{}, func(ctx context.Context, receipt targetContext) (string, error) {
		if receipt.ProjectWorkspaceID != "project-1" || receipt.RepositoryWorkspaceID != "repository-1" {
			t.Fatalf("operation receipt = %#v", receipt)
		}
		receipt.VisibleRepositoryWorkspaceIDs[0] = "mutated"
		receipt.Evidence[0].Value = "mutated"
		response, err := server.Client().Get(server.URL)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		return "routed", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "routed" || provider.calls != 1 || requests != 1 {
		t.Fatalf("result=%q provider calls=%d outbound requests=%d", got, provider.calls, requests)
	}
	if got := provider.result.Context.VisibleRepositoryWorkspaceIDs[0]; got != "repository-1" {
		t.Fatalf("operation mutated provider receipt visible repositories: %q", got)
	}
	if got := provider.result.Context.Evidence[0].Value; got != "git:example.test/acme/api" {
		t.Fatalf("operation mutated provider receipt evidence: %q", got)
	}
}

func TestWithResolvedTargetRefusesEveryNonResolvedResultWithoutRunningOperation(t *testing.T) {
	statuses := []resolutionStatus{resolutionUnresolved, resolutionAmbiguous, resolutionForbidden, resolutionStale}
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			provider := &fakeTargetProvider{result: resolutionResult{Status: status, Reason: "token and local path must stay redacted"}}
			assertTargetGateDoesNotCallOperation(t, provider, status)
		})
	}
}

func TestWithResolvedTargetRefusesResolverErrorWithoutRunningOperation(t *testing.T) {
	provider := &fakeTargetProvider{err: &targetResolutionError{status: resolutionForbidden, rung: "workspace_lookup"}}
	assertTargetGateDoesNotCallOperation(t, provider, resolutionForbidden)
}

func assertTargetGateDoesNotCallOperation(t *testing.T, provider *fakeTargetProvider, wantStatus resolutionStatus) {
	t.Helper()
	var requests, operations int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	_, err := withResolvedTarget(context.Background(), provider, resolveTargetInput{}, func(ctx context.Context, receipt targetContext) (struct{}, error) {
		operations++
		response, callErr := server.Client().Get(server.URL)
		if callErr != nil {
			return struct{}{}, callErr
		}
		defer response.Body.Close()
		return struct{}{}, nil
	})
	if err == nil {
		t.Fatal("gate accepted a non-resolved target")
	}
	var typed *targetResolutionError
	if !errors.As(err, &typed) || typed.status != wantStatus {
		t.Fatalf("error = %T %v, want redacted target resolution status %q", err, err, wantStatus)
	}
	if strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "path") {
		t.Fatalf("gate leaked resolution reason: %q", err)
	}
	if provider.calls != 1 || operations != 0 || requests != 0 {
		t.Fatalf("provider calls=%d operations=%d outbound requests=%d, want 1/0/0", provider.calls, operations, requests)
	}
}
