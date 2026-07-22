package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDoctorContextResolvedOutputIsRedacted(t *testing.T) {
	t.Setenv(workspaceIDEnv, "")
	t.Setenv("HOME", t.TempDir())
	const (
		token        = "lema_live_doctor_context_token_that_must_never_escape"
		projectID    = "11111111-2222-3333-4444-555555555555"
		repositoryID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		organization = "org-private-987654321"
		cwd          = "/private/home/andrew/work/payments-api"
		gitRoot      = "/private/home/andrew/work/payments-api"
		remote       = "https://token-in-url:password@github.com/acme/payments-api.git?access_token=leak"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("authorization = %q, want expected bearer token", got)
		}
		switch r.URL.Path {
		case "/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []map[string]any{
				{"id": repositoryID, "org_id": organization, "is_repo": true, "repo_url": "https://github.com/acme/payments-api.git"},
				{"id": projectID, "org_id": organization, "is_repo": false},
			}})
		case "/workspaces/" + projectID + "/links":
			_ = json.NewEncoder(w).Encode(map[string]any{"links": []map[string]any{{"workspace_id": repositoryID}}})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	var output bytes.Buffer
	err := runDoctorContext(context.Background(), doctorContextOptions{
		APIURL: server.URL,
		Token:  token,
		CWD:    cwd,
		ReadGit: func(context.Context, string) (gitTargetEvidence, error) {
			return gitTargetEvidence{RemoteURL: remote, Root: gitRoot}, nil
		},
		Output: &output,
	})
	if err != nil {
		t.Fatalf("runDoctorContext: %v", err)
	}

	got := output.String()
	for _, want := range []string{
		"result         resolved by canonical_git",
		"repository     git:github.com/acme/payments-api",
		"project        UUID ending …55555555",
		"repository id  UUID ending …eeeeeeee",
		"credential     sha256:…",
		"cwd hash       sha256:",
		"git root hash  sha256:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic missing %q:\n%s", want, got)
		}
	}
	for _, secret := range []string{token, remote, cwd, gitRoot, projectID, repositoryID, organization, credentialFingerprint(token)} {
		if strings.Contains(got, secret) {
			t.Errorf("diagnostic leaked %q:\n%s", secret, got)
		}
	}
	if strings.Contains(got, "usr_") {
		t.Errorf("diagnostic invented a user identity:\n%s", got)
	}
}

func TestDoctorContextFailureIsTypedAndHasOneSafeAction(t *testing.T) {
	t.Setenv(workspaceIDEnv, "")
	t.Setenv("HOME", t.TempDir())
	const token = "lema_live_unresolved_doctor_context_token"
	var output bytes.Buffer
	err := runDoctorContext(context.Background(), doctorContextOptions{
		APIURL: "https://api.example.test",
		Token:  token,
		CWD:    "/private/home/andrew/no-git-repository",
		ReadGit: func(context.Context, string) (gitTargetEvidence, error) {
			return gitTargetEvidence{}, nil
		},
		Output: &output,
	})
	if err == nil {
		t.Fatal("unresolved target returned success")
	}
	got := output.String()
	if !strings.Contains(got, "result         unresolved") {
		t.Errorf("status missing from diagnostic:\n%s", got)
	}
	if strings.Count(got, "action         ") != 1 {
		t.Errorf("diagnostic must print exactly one safe corrective action:\n%s", got)
	}
	if strings.Contains(got, token) || strings.Contains(got, "/private/home/andrew") || strings.Contains(got, credentialFingerprint(token)) {
		t.Errorf("failure diagnostic leaked sensitive local evidence:\n%s", got)
	}
}

func TestDoctorContextNonResolvedResultsHaveOneSafeAction(t *testing.T) {
	for _, status := range []resolutionStatus{resolutionUnresolved, resolutionAmbiguous, resolutionForbidden, resolutionStale} {
		t.Run(string(status), func(t *testing.T) {
			var output bytes.Buffer
			writeDoctorContext(&output, "lema_live_safe_diagnostic_token", resolutionResult{Status: status})
			got := output.String()
			if !strings.Contains(got, "result         "+string(status)) {
				t.Errorf("typed status missing:\n%s", got)
			}
			if strings.Count(got, "action         ") != 1 {
				t.Errorf("%s must have exactly one corrective action:\n%s", status, got)
			}
		})
	}
}
