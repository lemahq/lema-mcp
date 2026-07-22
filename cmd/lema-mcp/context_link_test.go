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

const (
	contextLinkProjectID = "11111111-2222-3333-4444-555555555555"
	contextLinkRepoID    = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

func contextLinkServer(t *testing.T, links []string, repositoryURL string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer lema_live_context_link_test" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []map[string]any{
				{"id": contextLinkRepoID, "org_id": "org-1", "is_repo": true, "repo_url": repositoryURL},
				{"id": contextLinkProjectID, "org_id": "org-1", "is_repo": false},
			}})
		case "/workspaces/" + contextLinkProjectID + "/links":
			values := make([]map[string]string, 0, len(links))
			for _, id := range links {
				values = append(values, map[string]string{"workspace_id": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"links": values})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func contextLinkCommandOptions(ts *httptest.Server, cwd string) contextLinkOptions {
	return contextLinkOptions{
		APIURL: ts.URL,
		Token:  "lema_live_context_link_test",
		CWD:    cwd,
		ReadGit: func(_ context.Context, _ string) (gitTargetEvidence, error) {
			return gitTargetEvidence{Root: cwd, RemoteURL: "https://github.com/acme/payments-api.git"}, nil
		},
	}
}

func TestContextLinkPersistsRedactedAssociationAndLoadsIt(t *testing.T) {
	root := t.TempDir()
	ts := contextLinkServer(t, []string{contextLinkRepoID}, "https://github.com/acme/payments-api.git")
	defer ts.Close()
	options := contextLinkCommandOptions(ts, root)

	if err := runContextLink(context.Background(), options, contextLinkProjectID, contextLinkRepoID); err != nil {
		t.Fatalf("link: %v", err)
	}
	path := filepath.Join(root, ".lema", "context.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{options.Token, options.APIURL, root, "acme/payments-api.git"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("association leaked %q: %s", forbidden, data)
		}
	}
	if !strings.Contains(string(data), "git:github.com/acme/payments-api") || !strings.Contains(string(data), "local_root_hash") {
		t.Fatalf("association missing canonical identity or root hash: %s", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}

	loaded, found, err := loadContextAssociation(root, options.ReadGit)
	if err != nil || !found {
		t.Fatalf("load = (%#v, %t, %v)", loaded, found, err)
	}
	if loaded.ProjectWorkspaceID != contextLinkProjectID || loaded.RepositoryWorkspaceID != contextLinkRepoID || loaded.Repository.Canonical != "git:github.com/acme/payments-api" {
		t.Fatalf("loaded association = %#v", loaded)
	}
}

func TestContextLinkRejectsCurrentGitMismatchWithoutWriting(t *testing.T) {
	root := t.TempDir()
	ts := contextLinkServer(t, []string{contextLinkRepoID}, "https://github.com/acme/payments-api.git")
	defer ts.Close()
	options := contextLinkCommandOptions(ts, root)
	options.ReadGit = func(context.Context, string) (gitTargetEvidence, error) {
		return gitTargetEvidence{Root: root, RemoteURL: "https://github.com/acme/other.git"}, nil
	}
	if err := runContextLink(context.Background(), options, contextLinkProjectID, contextLinkRepoID); !isContextAssociationStale(err) {
		t.Fatalf("link error = %v, want typed stale", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".lema", "context.json")); !os.IsNotExist(err) {
		t.Fatalf("mismatched link wrote association: %v", err)
	}
}

func TestContextLinkDefaultGitReaderRejectsUpstreamOnlyMismatch(t *testing.T) {
	root := t.TempDir()
	if output, err := exec.Command("git", "init", root).CombinedOutput(); err != nil {
		t.Skipf("git init unavailable: %v (%s)", err, output)
	}
	if output, err := exec.Command("git", "-C", root, "remote", "add", "upstream", "https://github.com/acme/other.git").CombinedOutput(); err != nil {
		t.Fatal(string(output))
	}
	ts := contextLinkServer(t, []string{contextLinkRepoID}, "https://github.com/acme/payments-api.git")
	defer ts.Close()
	options := contextLinkCommandOptions(ts, root)
	options.ReadGit = nil // Exercise the command's real Git adapter, not a fixture.
	if err := runContextLink(context.Background(), options, contextLinkProjectID, contextLinkRepoID); !isContextAssociationStale(err) {
		t.Fatalf("upstream-only mismatch = %v, want typed stale", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".lema", "context.json")); !os.IsNotExist(err) {
		t.Fatalf("upstream mismatch wrote association: %v", err)
	}
}

func TestContextLinkRejectsForeignOrHiddenRepositoryWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name       string
		workspaces []map[string]any
		links      []string
	}{
		{
			name: "foreign organization",
			workspaces: []map[string]any{
				{"id": contextLinkRepoID, "org_id": "org-foreign", "is_repo": true, "repo_url": "https://github.com/acme/payments-api.git"},
				{"id": contextLinkProjectID, "org_id": "org-1", "is_repo": false},
			},
			links: []string{contextLinkRepoID},
		},
		{
			name: "hidden repository",
			workspaces: []map[string]any{
				{"id": contextLinkProjectID, "org_id": "org-1", "is_repo": false},
			},
			links: []string{contextLinkRepoID},
		},
		{
			name: "missing organization identity",
			workspaces: []map[string]any{
				{"id": contextLinkRepoID, "is_repo": true, "repo_url": "https://github.com/acme/payments-api.git"},
				{"id": contextLinkProjectID, "is_repo": false},
			},
			links: []string{contextLinkRepoID},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/workspaces":
					_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": tc.workspaces})
				case "/workspaces/" + contextLinkProjectID + "/links":
					_ = json.NewEncoder(w).Encode(map[string]any{"links": []map[string]string{{"workspace_id": contextLinkRepoID}}})
				default:
					t.Fatalf("unexpected path %q", r.URL.Path)
				}
			}))
			defer server.Close()
			err := runContextLink(context.Background(), contextLinkCommandOptions(server, root), contextLinkProjectID, contextLinkRepoID)
			if err == nil {
				t.Fatal("foreign or hidden repository linked successfully")
			}
			if _, statErr := os.Stat(filepath.Join(root, ".lema", "context.json")); !os.IsNotExist(statErr) {
				t.Fatalf("rejected link persisted a file: %v", statErr)
			}
		})
	}
}

func TestContextAssociationRejectsSymlinkedDirectoryForEveryOperation(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(root, ".lema")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ts := contextLinkServer(t, []string{contextLinkRepoID}, "https://github.com/acme/payments-api.git")
	defer ts.Close()
	options := contextLinkCommandOptions(ts, root)

	if err := runContextLink(context.Background(), options, contextLinkProjectID, contextLinkRepoID); err == nil {
		t.Fatal("link accepted a symlinked .lema directory")
	}
	if _, err := os.Stat(filepath.Join(external, "context.json")); !os.IsNotExist(err) {
		t.Fatalf("link changed external target: %v", err)
	}
	if _, found, err := loadContextAssociation(root, options.ReadGit); err == nil || !found {
		t.Fatalf("load through symlink = found:%t err:%v, want refusal", found, err)
	}
	outside := filepath.Join(external, "context.json")
	if err := os.WriteFile(outside, []byte("outside association"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runContextUnlink(contextLinkOptions{CWD: root, ReadGit: options.ReadGit}); err == nil {
		t.Fatal("unlink accepted a symlinked .lema directory")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "outside association" {
		t.Fatalf("unlink changed external association: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(external, "context.json.bak")); !os.IsNotExist(err) {
		t.Fatalf("unlink created external backup: %v", err)
	}
}

func TestContextLinkSupportsNoRemoteAndNonGitAtRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(string) func(context.Context, string) (gitTargetEvidence, error)
	}{
		{"no remote Git", func(root string) func(context.Context, string) (gitTargetEvidence, error) {
			return func(context.Context, string) (gitTargetEvidence, error) { return gitTargetEvidence{Root: root}, nil }
		}},
		{"non Git", func(root string) func(context.Context, string) (gitTargetEvidence, error) {
			return func(context.Context, string) (gitTargetEvidence, error) { return gitTargetEvidence{}, nil }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			ts := contextLinkServer(t, []string{contextLinkRepoID}, "https://github.com/acme/payments-api.git")
			defer ts.Close()
			options := contextLinkCommandOptions(ts, root)
			options.ReadGit = tc.read(root)
			if err := runContextLink(context.Background(), options, contextLinkProjectID, contextLinkRepoID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, ".lema", "context.json")); err != nil {
				t.Fatalf("association did not use expected root: %v", err)
			}
		})
	}
}

func TestContextLinkNestedCWDPersistsAtGitRootAndRelocationIsStale(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested", "dir")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := contextLinkServer(t, []string{contextLinkRepoID}, "https://github.com/acme/payments-api.git")
	defer ts.Close()
	options := contextLinkCommandOptions(ts, nested)
	options.ReadGit = func(context.Context, string) (gitTargetEvidence, error) {
		return gitTargetEvidence{Root: root, RemoteURL: "https://github.com/acme/payments-api.git"}, nil
	}
	if err := runContextLink(context.Background(), options, contextLinkProjectID, contextLinkRepoID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".lema", "context.json")); err != nil {
		t.Fatalf("root association missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nested, ".lema", "context.json")); !os.IsNotExist(err) {
		t.Fatalf("nested association unexpectedly exists: %v", err)
	}

	relocated := t.TempDir()
	if err := os.MkdirAll(filepath.Join(relocated, ".lema"), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".lema", "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(relocated, ".lema", "context.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, found, err := loadContextAssociation(relocated, func(context.Context, string) (gitTargetEvidence, error) {
		return gitTargetEvidence{Root: relocated, RemoteURL: "https://github.com/acme/payments-api.git"}, nil
	})
	if !found || !isContextAssociationStale(err) {
		t.Fatalf("relocated association = found:%t err:%v, want stale", found, err)
	}
}

func TestContextLinkLoadRejectsCanonicalMismatch(t *testing.T) {
	root := t.TempDir()
	ts := contextLinkServer(t, []string{contextLinkRepoID}, "https://github.com/acme/payments-api.git")
	defer ts.Close()
	options := contextLinkCommandOptions(ts, root)
	if err := runContextLink(context.Background(), options, contextLinkProjectID, contextLinkRepoID); err != nil {
		t.Fatal(err)
	}
	_, found, err := loadContextAssociation(root, func(context.Context, string) (gitTargetEvidence, error) {
		return gitTargetEvidence{Root: root, RemoteURL: "https://github.com/acme/renamed-or-other.git"}, nil
	})
	if !found || !isContextAssociationStale(err) {
		t.Fatalf("canonical mismatch = found:%t err:%v, want stale", found, err)
	}
}

func TestContextLinkExplicitWorkspaceTakesPrecedenceOverSavedAssociation(t *testing.T) {
	root := t.TempDir()
	ts := contextLinkServer(t, []string{contextLinkRepoID}, "https://github.com/acme/payments-api.git")
	defer ts.Close()
	options := contextLinkCommandOptions(ts, root)
	if err := runContextLink(context.Background(), options, contextLinkProjectID, contextLinkRepoID); err != nil {
		t.Fatal(err)
	}
	// Make the saved association stale for this checkout. The explicit legacy
	// workspace remains the higher-precedence, separately validated input.
	options.ReadGit = func(context.Context, string) (gitTargetEvidence, error) {
		return gitTargetEvidence{Root: root, RemoteURL: "https://github.com/acme/other.git"}, nil
	}
	var output bytes.Buffer
	err := runDoctorContext(context.Background(), doctorContextOptions{
		APIURL:              ts.URL,
		Token:               options.Token,
		ExplicitWorkspaceID: contextLinkRepoID,
		CWD:                 root,
		ReadGit:             options.ReadGit,
		Output:              &output,
	})
	if err != nil || !strings.Contains(output.String(), "result         resolved by explicit") {
		t.Fatalf("explicit override = %v:\n%s", err, output.String())
	}
}

func TestContextUnlinkKeepsRecoverableUniqueBackup(t *testing.T) {
	root := t.TempDir()
	ts := contextLinkServer(t, []string{contextLinkRepoID}, "https://github.com/acme/payments-api.git")
	defer ts.Close()
	options := contextLinkCommandOptions(ts, root)
	if err := runContextLink(context.Background(), options, contextLinkProjectID, contextLinkRepoID); err != nil {
		t.Fatal(err)
	}
	if err := runContextUnlink(contextLinkOptions{CWD: root, ReadGit: options.ReadGit}); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".lema", "context.json.bak")); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if err := runContextLink(context.Background(), options, contextLinkProjectID, contextLinkRepoID); err != nil {
		t.Fatal(err)
	}
	if err := runContextUnlink(contextLinkOptions{CWD: root, ReadGit: options.ReadGit}); err != nil {
		t.Fatalf("second unlink: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".lema", "context.json.bak.1")); err != nil {
		t.Fatalf("numbered backup missing: %v", err)
	}
	if err := runContextUnlink(contextLinkOptions{CWD: root, ReadGit: options.ReadGit}); err == nil {
		t.Fatal("unlink without association succeeded")
	}
}

func TestDoctorContextUsesLinkedAssociationToResolveAmbiguity(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workspaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": []map[string]any{
				{"id": contextLinkRepoID, "org_id": "org-1", "is_repo": true, "repo_url": "https://github.com/acme/payments-api.git"},
				{"id": contextLinkProjectID, "org_id": "org-1", "is_repo": false},
				{"id": "22222222-2222-3333-4444-555555555555", "org_id": "org-1", "is_repo": false},
			}})
		case "/workspaces/" + contextLinkProjectID + "/links", "/workspaces/22222222-2222-3333-4444-555555555555/links":
			_ = json.NewEncoder(w).Encode(map[string]any{"links": []map[string]string{{"workspace_id": contextLinkRepoID}}})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	readGit := func(context.Context, string) (gitTargetEvidence, error) {
		return gitTargetEvidence{Root: root, RemoteURL: "https://github.com/acme/payments-api.git"}, nil
	}
	var before bytes.Buffer
	err := runDoctorContext(context.Background(), doctorContextOptions{APIURL: server.URL, Token: "lema_live_context_link_test", CWD: root, ReadGit: readGit, Output: &before})
	if err == nil || !strings.Contains(before.String(), "result         ambiguous") {
		t.Fatalf("doctor before link = %v:\n%s", err, before.String())
	}
	if err := runContextLink(context.Background(), contextLinkCommandOptions(server, root), contextLinkProjectID, contextLinkRepoID); err != nil {
		t.Fatal(err)
	}
	var after bytes.Buffer
	err = runDoctorContext(context.Background(), doctorContextOptions{APIURL: server.URL, Token: "lema_live_context_link_test", CWD: root, ReadGit: readGit, Output: &after})
	if err != nil || !strings.Contains(after.String(), "result         resolved by local_association") {
		t.Fatalf("doctor after link = %v:\n%s", err, after.String())
	}
	if !strings.Contains(after.String(), "local root hash  sha256:") {
		t.Fatalf("doctor dropped stored local-root evidence:\n%s", after.String())
	}
}
