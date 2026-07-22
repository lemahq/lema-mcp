package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
		"identity       unavailable",
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
	if !strings.Contains(got, "action         set LEMA_WORKSPACE_ID to a visible repository workspace") {
		t.Errorf("actual target absence must retain target-pin action:\n%s", got)
	}
	if strings.Contains(got, token) || strings.Contains(got, "/private/home/andrew") || strings.Contains(got, credentialFingerprint(token)) {
		t.Errorf("failure diagnostic leaked sensitive local evidence:\n%s", got)
	}
}

func TestDoctorContextLookupFailuresRecommendHostedCompatibility(t *testing.T) {
	const token = "lema_live_doctor_lookup_failure_token"
	workspaces := []map[string]any{
		{"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "org_id": "org-1", "is_repo": true, "repo_url": "https://github.com/acme/payments-api.git"},
		{"id": "11111111-2222-3333-4444-555555555555", "org_id": "org-1", "is_repo": false},
	}
	for _, tc := range []struct {
		name    string
		client  *http.Client
		handler http.HandlerFunc
		rung    string
	}{
		{
			name: "workspace transport",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, os.ErrDeadlineExceeded
			})},
			rung: "workspace_lookup",
		},
		{
			name:    "workspace non-auth HTTP",
			handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) },
			rung:    "workspace_lookup",
		},
		{
			name:    "workspace decode",
			handler: func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("not-json")) },
			rung:    "workspace_lookup",
		},
		{
			name: "link non-auth HTTP",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/workspaces" {
					_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": workspaces})
					return
				}
				w.WriteHeader(http.StatusBadGateway)
			},
			rung: "project_link_lookup",
		},
		{
			name: "link decode",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/workspaces" {
					_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": workspaces})
					return
				}
				_, _ = w.Write([]byte("not-json"))
			},
			rung: "project_link_lookup",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.client
			apiURL := "https://api.example.test"
			var server *httptest.Server
			if tc.handler != nil {
				server = httptest.NewServer(tc.handler)
				defer server.Close()
				client, apiURL = server.Client(), server.URL
			}
			var output bytes.Buffer
			err := runDoctorContext(context.Background(), doctorContextOptions{
				APIURL: apiURL,
				Token:  token,
				CWD:    "/private/home/andrew/work/payments-api",
				ReadGit: func(context.Context, string) (gitTargetEvidence, error) {
					return gitTargetEvidence{RemoteURL: "https://github.com/acme/payments-api.git", Root: "/private/home/andrew/work/payments-api"}, nil
				},
				Output: &output,
				Client: client,
			})
			if err == nil {
				t.Fatal("lookup failure returned success")
			}
			got := output.String()
			if !strings.Contains(got, "rung           "+tc.rung) || !strings.Contains(got, "result         unresolved") || !strings.Contains(got, "action         verify hosted API reachability and response compatibility") {
				t.Fatalf("lookup failure lacks typed hosted correction:\n%s", got)
			}
			if strings.Count(got, "action         ") != 1 || strings.Contains(got, workspaceIDEnv) {
				t.Fatalf("lookup failure must have exactly one non-target-pin action:\n%s", got)
			}
		})
	}
}

func TestResolveDoctorConfigKeepsCompleteEnvironmentWhenFallbackFileErrors(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".config"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("LEMA_API_URL", "https://env.example.test")
	t.Setenv("LEMA_API_TOKEN", "lema_live_env_token")
	t.Setenv(workspaceIDEnv, "env-workspace")

	config, err := resolveDoctorConfig()
	if err != nil {
		t.Fatalf("complete environment must not depend on unreadable fallback credentials: %v", err)
	}
	if config.APIURL != "https://env.example.test" || config.Token != "lema_live_env_token" || config.ExplicitWorkspaceID != "env-workspace" {
		t.Fatalf("config = %#v, want complete environment values", config)
	}
}

func TestDoctorContextNonResolvedResultsHaveOneSafeAction(t *testing.T) {
	for _, status := range []resolutionStatus{resolutionUnresolved, resolutionAmbiguous, resolutionForbidden, resolutionStale} {
		t.Run(string(status), func(t *testing.T) {
			var output bytes.Buffer
			writeDoctorContext(&output, resolutionResult{Status: status})
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

func TestDoctorContextAmbiguityActionSelectsProjectAndRepository(t *testing.T) {
	action := doctorCorrectiveAction(resolutionAmbiguous)
	if !strings.Contains(action, "lema-mcp context link --project <project-id> --repository <repository-id>") {
		t.Fatalf("ambiguous action = %q, want a project-and-repository association command", action)
	}

	base := resolverFixture(t)
	base.parents = []string{"project-payments", "project-platform"}
	ambiguous, err := base.resolver().Resolve(context.Background(), resolveTargetInput{OrganizationID: "org-1", CWD: "/repo"})
	if err != nil || ambiguous.Status != resolutionAmbiguous || len(ambiguous.Candidates) != 2 {
		t.Fatalf("ambiguous resolution = %#v, %v", ambiguous, err)
	}
	association := ambiguous.Candidates[0]
	resolved, err := base.resolver().Resolve(context.Background(), resolveTargetInput{OrganizationID: "org-1", CWD: "/no-git", LocalAssociation: &association})
	if err != nil || resolved.Status != resolutionResolved || resolved.Context.ProjectWorkspaceID != association.ProjectWorkspaceID || resolved.Context.RepositoryWorkspaceID != association.RepositoryWorkspaceID {
		t.Fatalf("association candidate must resolve the selected project/repository pair: %#v, %v", resolved, err)
	}
}

func TestDoctorContextNormalizesLegacyWorkspaceSlugPins(t *testing.T) {
	const (
		token        = "lema_live_doctor_slug_pin_token"
		repositoryID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		projectID    = "11111111-2222-3333-4444-555555555555"
		slug         = "payments-api"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []map[string]any{
				{"id": repositoryID, "slug": slug, "org_id": "org-1", "is_repo": true, "repo_url": "https://github.com/acme/payments-api.git"},
				{"id": projectID, "org_id": "org-1", "is_repo": false},
			}})
		case "/workspaces/" + projectID + "/links":
			_ = json.NewEncoder(w).Encode(map[string]any{"links": []map[string]any{{"workspace_id": repositoryID}}})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	for _, tc := range []struct {
		name  string
		setup func(*testing.T)
	}{
		{"environment", func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("LEMA_API_URL", server.URL)
			t.Setenv("LEMA_API_TOKEN", token)
			t.Setenv(workspaceIDEnv, slug)
		}},
		{"credentials file", func(t *testing.T) {
			t.Setenv("LEMA_API_URL", "")
			t.Setenv("LEMA_API_TOKEN", "")
			t.Setenv(workspaceIDEnv, "")
			writeCredsFile(t, "LEMA_API_URL="+server.URL+"\nLEMA_API_TOKEN="+token+"\n"+workspaceIDEnv+"="+slug+"\n", 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			config, err := resolveDoctorConfig()
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err = runDoctorContext(context.Background(), doctorContextOptions{
				APIURL:              config.APIURL,
				Token:               config.Token,
				ExplicitWorkspaceID: config.ExplicitWorkspaceID,
				CWD:                 "/private/home/andrew/work/payments-api",
				ReadGit: func(context.Context, string) (gitTargetEvidence, error) {
					return gitTargetEvidence{RemoteURL: "https://github.com/acme/payments-api.git", Root: "/private/home/andrew/work/payments-api"}, nil
				},
				Output: &output,
			})
			if err != nil {
				t.Fatalf("slug pin = %v:\n%s", err, output.String())
			}
			if !strings.Contains(output.String(), "result         resolved by explicit") {
				t.Fatalf("slug pin did not use explicit resolution:\n%s", output.String())
			}
		})
	}
}

func TestDoctorContextClassifiesHostedAuthorizationFailure(t *testing.T) {
	const token = "lema_live_doctor_forbidden_token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("credential and URL must not be reported"))
	}))
	defer server.Close()

	var output bytes.Buffer
	err := runDoctorContext(context.Background(), doctorContextOptions{
		APIURL: server.URL,
		Token:  token,
		CWD:    "/private/home/andrew/work/payments-api",
		ReadGit: func(context.Context, string) (gitTargetEvidence, error) {
			return gitTargetEvidence{RemoteURL: "https://github.com/acme/payments-api.git", Root: "/private/home/andrew/work/payments-api"}, nil
		},
		Output: &output,
	})
	if err == nil {
		t.Fatal("forbidden lookup returned success")
	}
	got := output.String()
	if !strings.Contains(got, "rung           workspace_lookup") || !strings.Contains(got, "result         forbidden") || !strings.Contains(got, "action         switch to a credential authorized for this repository") {
		t.Fatalf("authorization failure was not typed safely:\n%s", got)
	}
	for _, secret := range []string{token, server.URL, "/private/home/andrew", "credential and URL must not be reported"} {
		if strings.Contains(got, secret) {
			t.Errorf("authorization diagnostic leaked %q:\n%s", secret, got)
		}
	}
}

func TestDoctorContextLooseCredentialsNeverPrintsRawHomePath(t *testing.T) {
	home := t.TempDir()
	credentialsDir := filepath.Join(home, ".config", "lema")
	if err := os.MkdirAll(credentialsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialsDir, "credentials"), []byte("LEMA_API_URL=https://127.0.0.1:1\nLEMA_API_TOKEN=lema_live_loose_file_token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "lema-mcp")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build doctor binary: %v\n%s", err, output)
	}
	cmd := exec.Command(binary, "doctor", "context")
	cmd.Dir = t.TempDir()
	cmd.Env = append(withoutEnv(os.Environ(), "HOME", "LEMA_API_URL", "LEMA_API_TOKEN", workspaceIDEnv), "HOME="+home)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("doctor unexpectedly succeeded without a reachable hosted API")
	}
	if strings.Contains(string(output), home) {
		t.Fatalf("doctor stderr/stdout leaked raw home path %q:\n%s", home, output)
	}
}

func withoutEnv(env []string, names ...string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		keep := true
		for _, name := range names {
			if key == name {
				keep = false
				break
			}
		}
		if keep {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
