package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTargetResolverPrecedence(t *testing.T) {
	base := resolverFixture(t)
	explicitRepo := targetWorkspace{ID: "repo-explicit", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/acme/explicit.git")}
	base.workspaces = append(base.workspaces,
		explicitRepo,
		targetWorkspace{ID: "run-project", OrganizationID: "org-1"},
		targetWorkspace{ID: "repo-run", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/acme/run.git")},
		targetWorkspace{ID: "repo-git", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/acme/git.git")},
		targetWorkspace{ID: "local-project", OrganizationID: "org-1"},
		targetWorkspace{ID: "repo-local", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/acme/local.git")},
	)
	base.run = resolvedTarget("run-project", "repo-run", "https://github.com/acme/run.git", "run")
	base.git = gitTargetEvidence{RemoteURL: "https://github.com/acme/git.git", Root: "/repos/git"}
	base.local = &targetContext{OrganizationID: "org-1", ProjectWorkspaceID: "local-project", RepositoryWorkspaceID: "repo-local", Repository: mustRepository(t, "https://github.com/acme/local.git")}

	cases := []struct {
		name string
		in   resolveTargetInput
		want string
	}{
		{"explicit beats every lower rung", resolveTargetInput{ExplicitWorkspaceID: "repo-explicit", RunID: "run-1", CWD: "/repos/git", LocalAssociation: base.local}, "explicit"},
		{"run beats git and association", resolveTargetInput{RunID: "run-1", CWD: "/repos/git", LocalAssociation: base.local}, "run"},
		{"canonical git beats association", resolveTargetInput{CWD: "/repos/git", LocalAssociation: base.local}, "canonical_git"},
		{"local association follows absent git", resolveTargetInput{CWD: "/no-git", LocalAssociation: base.local}, "local_association"},
		{"no evidence remains unresolved", resolveTargetInput{CWD: "/no-git"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := base.resolver().Resolve(context.Background(), tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if result.Status != resolutionUnresolved {
					t.Fatalf("status = %q, want unresolved", result.Status)
				}
				return
			}
			if result.Status != resolutionResolved || result.Context.ResolvedBy != tc.want {
				t.Fatalf("result = %#v, want resolved by %q", result, tc.want)
			}
		})
	}
}

func TestTargetResolverExplicitFailureStopsBeforeGit(t *testing.T) {
	base := resolverFixture(t)
	base.git = gitTargetEvidence{RemoteURL: "https://github.com/acme/repo.git", Root: "/repo"}
	for _, tc := range []struct {
		name string
		in   resolveTargetInput
		want resolutionStatus
	}{
		{"stale workspace", resolveTargetInput{ExplicitWorkspaceID: "removed-workspace", CWD: "/repo"}, resolutionStale},
		{"forbidden repository", resolveTargetInput{ExplicitRepository: mustRepository(t, "https://github.com/acme/not-visible.git"), CWD: "/repo"}, resolutionForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base.gitCalls = 0
			result, err := base.resolver().Resolve(context.Background(), tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != tc.want {
				t.Fatalf("status = %q, want %q (%s)", result.Status, tc.want, result.Reason)
			}
			if base.gitCalls != 0 {
				t.Fatalf("Git reader called %d times after an explicit %s result", base.gitCalls, tc.want)
			}
		})
	}
}

func TestTargetResolverProjectParents(t *testing.T) {
	for _, tc := range []struct {
		name        string
		parents     []string
		wantStatus  resolutionStatus
		wantProject string
		wantCount   int
	}{
		{"unlinked repository is a singleton project", nil, resolutionResolved, "repo-api", 0},
		{"one visible parent resolves", []string{"project-payments"}, resolutionResolved, "project-payments", 0},
		{"two visible parents are ambiguous", []string{"project-payments", "project-platform"}, resolutionAmbiguous, "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := resolverFixture(t)
			base.parents = tc.parents
			result, err := base.resolver().Resolve(context.Background(), resolveTargetInput{CWD: "/repo"})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, tc.wantStatus)
			}
			if tc.wantProject != "" && result.Context.ProjectWorkspaceID != tc.wantProject {
				t.Fatalf("project = %q, want %q", result.Context.ProjectWorkspaceID, tc.wantProject)
			}
			if len(result.Candidates) != tc.wantCount {
				t.Fatalf("candidates = %#v, want %d", result.Candidates, tc.wantCount)
			}
		})
	}
}

func TestTargetResolverRepositoryIdentityIncludesHost(t *testing.T) {
	base := resolverFixture(t)
	base.workspaces = append(base.workspaces, targetWorkspace{ID: "repo-enterprise", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "ssh://git@github.acme.internal/acme/api.git")})
	for _, tc := range []struct {
		remote, wantID, wantCanonical string
	}{
		{"https://github.com/acme/api.git", "repo-api", "git:github.com/acme/api"},
		{"git@github.acme.internal:acme/api.git", "repo-enterprise", "git:github.acme.internal/acme/api"},
	} {
		base.git = gitTargetEvidence{RemoteURL: tc.remote, Root: "/repo"}
		result, err := base.resolver().Resolve(context.Background(), resolveTargetInput{CWD: "/repo"})
		if err != nil {
			t.Fatal(err)
		}
		if result.Context.RepositoryWorkspaceID != tc.wantID || result.Context.Repository.Canonical != tc.wantCanonical {
			t.Fatalf("result = %#v, want workspace=%q canonical=%q", result, tc.wantID, tc.wantCanonical)
		}
	}
}

func TestTargetResolverWorktreesAndNestedCWDKeepRepositoryButHashPaths(t *testing.T) {
	base := resolverFixture(t)
	base.readGit = func(_ context.Context, cwd string) (gitTargetEvidence, error) {
		return gitTargetEvidence{RemoteURL: "https://github.com/acme/api.git", Root: strings.TrimSuffix(cwd, "/nested")}, nil
	}
	paths := []string{"/worktrees/api-one", "/worktrees/api-two", "/worktrees/api-one/nested"}
	seen := map[string]bool{}
	for _, cwd := range paths {
		result, err := base.resolver().Resolve(context.Background(), resolveTargetInput{CWD: cwd})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != resolutionResolved || result.Context.Repository.Canonical != "git:github.com/acme/api" {
			t.Fatalf("cwd %q resolved %#v", cwd, result)
		}
		hash := evidenceValue(result.Context.Evidence, "cwd_path_hash")
		if hash == "" || seen[hash] {
			t.Fatalf("cwd %q has non-distinct path evidence %q", cwd, hash)
		}
		seen[hash] = true
	}
}

func TestTargetResolverCacheIsBoundedAndCredentialScoped(t *testing.T) {
	base := resolverFixture(t)
	base.cache = newTargetResolutionCache(2)
	base.git = gitTargetEvidence{RemoteURL: "https://github.com/acme/api.git", Root: "/repo"}
	inputs := []resolveTargetInput{
		{APIURL: "https://api.one", CredentialFingerprint: "cred-a", CWD: "/repo"},
		{APIURL: "https://api.one", CredentialFingerprint: "cred-a", CWD: "/repo"}, // same entry
		{APIURL: "https://api.two", CredentialFingerprint: "cred-a", CWD: "/repo"},
		{APIURL: "https://api.one", CredentialFingerprint: "cred-b", CWD: "/repo"},
	}
	for _, in := range inputs {
		if _, err := base.resolver().Resolve(context.Background(), in); err != nil {
			t.Fatal(err)
		}
	}
	if base.workspaceCalls != 3 {
		t.Fatalf("workspace fetches = %d, want 3: API URL and credential fingerprints must never share cache entries", base.workspaceCalls)
	}
	if got := base.cache.len(); got > 2 {
		t.Fatalf("cache size = %d, want bounded at 2", got)
	}
}

type targetResolverFixture struct {
	workspaces     []targetWorkspace
	parents        []string
	run            targetContext
	git            gitTargetEvidence
	local          *targetContext
	readGit        func(context.Context, string) (gitTargetEvidence, error)
	cache          *targetResolutionCache
	gitCalls       int
	workspaceCalls int
}

func resolverFixture(t *testing.T) *targetResolverFixture {
	t.Helper()
	return &targetResolverFixture{
		workspaces: []targetWorkspace{
			{ID: "repo-api", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/acme/api.git")},
			{ID: "project-payments", OrganizationID: "org-1"},
			{ID: "project-platform", OrganizationID: "org-1"},
		},
		git:   gitTargetEvidence{RemoteURL: "https://github.com/acme/api.git", Root: "/repo"},
		cache: newTargetResolutionCache(64),
	}
}

func (f *targetResolverFixture) resolver() *targetResolver {
	readGit := f.readGit
	if readGit == nil {
		readGit = func(_ context.Context, cwd string) (gitTargetEvidence, error) {
			f.gitCalls++
			if cwd == "/no-git" {
				return gitTargetEvidence{}, errNoGitEvidence
			}
			if f.git.RemoteURL == "" && f.git.Repository.Canonical == "" {
				return gitTargetEvidence{}, errNoGitEvidence
			}
			return f.git, nil
		}
	}
	return &targetResolver{
		fetchWorkspaces: func(context.Context, resolveTargetInput) ([]targetWorkspace, error) {
			f.workspaceCalls++
			return f.workspaces, nil
		},
		fetchLinks: func(_ context.Context, _ resolveTargetInput, projectID string) ([]string, error) {
			switch projectID {
			case "run-project":
				return []string{"repo-run"}, nil
			case "local-project":
				return []string{"repo-local"}, nil
			}
			for _, parent := range f.parents {
				if parent == projectID {
					return []string{"repo-api"}, nil
				}
			}
			return nil, nil
		},
		resumeRun: func(context.Context, resolveTargetInput, string) (resolutionResult, error) {
			if f.run.RepositoryWorkspaceID == "" {
				return resolutionResult{Status: resolutionUnresolved}, nil
			}
			return resolutionResult{Status: resolutionResolved, Context: f.run}, nil
		},
		readGit: readGit,
		now:     func() time.Time { return time.Unix(1, 0).UTC() },
		cache:   f.cache,
	}
}

func mustRepository(t *testing.T, remote string) repositoryIdentity {
	t.Helper()
	repo, ok := repositoryIdentityFromRemote(remote)
	if !ok {
		t.Fatalf("repositoryIdentityFromRemote(%q) failed", remote)
	}
	return repo
}

func resolvedTarget(project, repo, remote, by string) targetContext {
	identity, _ := repositoryIdentityFromRemote(remote)
	return targetContext{OrganizationID: "org-1", ProjectWorkspaceID: project, RepositoryWorkspaceID: repo, Repository: identity, ResolvedBy: by}
}

func evidenceValue(evidence []resolutionEvidence, kind string) string {
	for _, item := range evidence {
		if item.Kind == kind {
			return item.Value
		}
	}
	return ""
}

var errNoGitEvidence = fmt.Errorf("no git evidence")
