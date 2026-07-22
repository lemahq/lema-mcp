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
	if base.workspaceCalls != 4 {
		t.Fatalf("workspace fetches = %d, want 4: every cache lookup revalidates current authority and API URL/credential fingerprints remain isolated", base.workspaceCalls)
	}
	if got := base.cache.len(); got > 2 {
		t.Fatalf("cache size = %d, want bounded at 2", got)
	}
}

func TestTargetResolverCacheRevalidatesWarmReceipt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*targetResolverFixture)
	}{
		{"repository visibility revoked", func(base *targetResolverFixture) {
			base.workspaces = removeWorkspace(base.workspaces, "repo-api")
		}},
		{"project link revoked", func(base *targetResolverFixture) {
			base.parents = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := resolverFixture(t)
			base.parents = []string{"project-payments"}
			in := resolveTargetInput{APIURL: "https://api.example", CredentialFingerprint: "cred-a", OrganizationID: "org-1", CWD: "/repo"}
			first, err := base.resolver().Resolve(context.Background(), in)
			if err != nil || first.Status != resolutionResolved {
				t.Fatalf("warm resolution = %#v, %v", first, err)
			}
			before := base.workspaceCalls
			tc.mutate(base)
			second, err := base.resolver().Resolve(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if second.Status == resolutionResolved {
				t.Fatalf("warm cache returned a resolved context after authoritative %s: %#v", tc.name, second)
			}
			if base.workspaceCalls <= before {
				t.Fatalf("cache hit did not re-fetch authoritative visibility: before=%d after=%d", before, base.workspaceCalls)
			}
		})
	}
}

func TestTargetResolverCacheRequiresFingerprintAndExpires(t *testing.T) {
	base := resolverFixture(t)
	base.cache = newTargetResolutionCache(2)
	base.now = time.Unix(1, 0).UTC()
	in := resolveTargetInput{APIURL: "https://api.example", CWD: "/repo"}
	if _, err := base.resolver().Resolve(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if got := base.cache.len(); got != 0 {
		t.Fatalf("cache size without credential fingerprint = %d, want 0", got)
	}
	in.CredentialFingerprint = "cred-a"
	if _, err := base.resolver().Resolve(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if got := base.cache.len(); got != 1 {
		t.Fatalf("cache size with credential fingerprint = %d, want 1", got)
	}
	base.now = base.now.Add(targetCacheTTL + time.Second)
	if _, ok := base.cache.get(base.resolver().cacheKey(in, mustRepository(t, "https://github.com/acme/api.git")), base.now); ok {
		t.Fatal("expired cache entry remained usable")
	}
}

func TestTargetResolverRebuildsReceiptsFromCurrentAuthority(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input func(targetContext) resolveTargetInput
	}{
		{"run", func(receipt targetContext) resolveTargetInput {
			return resolveTargetInput{OrganizationID: "org-1", RunID: "run-1"}
		}},
		{"local association", func(receipt targetContext) resolveTargetInput {
			return resolveTargetInput{OrganizationID: "org-1", CWD: "/no-git", LocalAssociation: &receipt}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := resolverFixture(t)
			base.workspaces = append(base.workspaces, targetWorkspace{ID: "repo-web", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/acme/web.git")})
			base.links = map[string][]string{"project-payments": {"repo-api", "repo-web"}}
			receipt := resolvedTarget("project-payments", "repo-api", "https://github.com/acme/api.git", "prior")
			receipt.VisibleRepositoryWorkspaceIDs = []string{"repo-api", "repo-web"}
			receipt.Evidence = []resolutionEvidence{{Kind: "prior", Value: "immutable"}}
			base.run = receipt
			base.workspaces = removeWorkspace(base.workspaces, "repo-web")

			result, err := base.resolver().Resolve(context.Background(), tc.input(receipt))
			if err != nil || result.Status != resolutionResolved {
				t.Fatalf("result = %#v, %v", result, err)
			}
			if got, want := strings.Join(result.Context.VisibleRepositoryWorkspaceIDs, ","), "repo-api"; got != want {
				t.Fatalf("visible repositories = %q, want %q after secondary visibility revocation", got, want)
			}
			result.Context.VisibleRepositoryWorkspaceIDs[0] = "mutated"
			result.Context.Evidence[0].Value = "mutated"
			if got := strings.Join(receipt.VisibleRepositoryWorkspaceIDs, ","); got != "repo-api,repo-web" || receipt.Evidence[0].Value != "immutable" {
				t.Fatalf("returned context aliases caller receipt: %#v", receipt)
			}
		})
	}
}

func TestTargetResolverRejectsArchivedOrRepositoryProjectReceipts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*targetWorkspace)
	}{
		{"archived", func(project *targetWorkspace) { project.Archived = true }},
		{"repository leaf", func(project *targetWorkspace) { project.IsRepository = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := resolverFixture(t)
			base.links = map[string][]string{"project-payments": {"repo-api"}}
			base.run = resolvedTarget("project-payments", "repo-api", "https://github.com/acme/api.git", "prior")
			for i := range base.workspaces {
				if base.workspaces[i].ID == "project-payments" {
					tc.mutate(&base.workspaces[i])
				}
			}
			result, err := base.resolver().Resolve(context.Background(), resolveTargetInput{OrganizationID: "org-1", RunID: "run-1"})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != resolutionStale {
				t.Fatalf("status = %q, want stale for %s project", result.Status, tc.name)
			}
		})
	}
}

func TestTargetResolverRequiresLinkAuthority(t *testing.T) {
	base := resolverFixture(t)
	resolver := base.resolver()
	resolver.fetchLinks = nil
	result, err := resolver.Resolve(context.Background(), resolveTargetInput{OrganizationID: "org-1", CWD: "/repo"})
	if err == nil && result.Status == resolutionResolved {
		t.Fatalf("missing link authority resolved singleton context: %#v", result)
	}
}

func TestTargetResolverSingletonReceiptRequiresLinkAuthority(t *testing.T) {
	base := resolverFixture(t)
	base.run = resolvedTarget("repo-api", "repo-api", "https://github.com/acme/api.git", "prior")
	resolver := base.resolver()
	resolver.fetchLinks = nil
	result, err := resolver.Resolve(context.Background(), resolveTargetInput{OrganizationID: "org-1", RunID: "run-1"})
	if err == nil && result.Status == resolutionResolved {
		t.Fatalf("missing link authority revalidated a singleton receipt: %#v", result)
	}
}

func TestTargetResolverNeverCrossesOrganization(t *testing.T) {
	base := resolverFixture(t)
	for i := range base.workspaces {
		if base.workspaces[i].ID == "repo-api" {
			base.workspaces[i].OrganizationID = "org-foreign"
		}
	}
	for _, in := range []resolveTargetInput{
		{OrganizationID: "org-1", CWD: "/repo"},
		{OrganizationID: "org-1", ExplicitWorkspaceID: "repo-api"},
	} {
		result, err := base.resolver().Resolve(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status == resolutionResolved {
			t.Fatalf("foreign workspace resolved under selected organization: %#v", result)
		}
	}
}

func TestRepositoryIdentityFromRemoteRedactsAndNormalizes(t *testing.T) {
	for _, tc := range []struct {
		remote string
		want   string
		ok     bool
	}{
		{"https://token:secret@GitHub.com:443/acme/api.git?token=leak#fragment", "git:github.com/acme/api", true},
		{"ssh://git@github.com:22/acme/api.git?token=leak#fragment", "git:github.com/acme/api", true},
		{"git@github.com:acme/api.git?token=leak#fragment", "git:github.com/acme/api", true},
		{"https://github.com:8443/acme/api.git", "git:github.com:8443/acme/api", true},
		{"ftp://github.com/acme/api.git", "", false},
	} {
		identity, ok := repositoryIdentityFromRemote(tc.remote)
		if ok != tc.ok || identity.Canonical != tc.want {
			t.Errorf("repositoryIdentityFromRemote(%q) = (%#v,%v), want canonical=%q ok=%v", tc.remote, identity, ok, tc.want, tc.ok)
		}
		if strings.Contains(identity.Canonical, "secret") || strings.Contains(identity.Canonical, "leak") || strings.Contains(identity.Canonical, "#") || strings.Contains(identity.Canonical, "?") {
			t.Errorf("canonical identity leaked remote secret/suffix: %q", identity.Canonical)
		}
	}
}

func TestTargetResolverExplicitProjectUsesVisibleLeafIntersection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		links      []string
		addWeb     bool
		wantStatus resolutionStatus
		wantRepo   string
		wantCount  int
	}{
		{"one visible linked leaf", []string{"repo-api"}, false, resolutionResolved, "repo-api", 0},
		{"no visible linked leaf", []string{"hidden-repo"}, false, resolutionUnresolved, "", 0},
		{"multiple visible linked leaves", []string{"repo-api", "repo-web"}, true, resolutionAmbiguous, "", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := resolverFixture(t)
			if tc.addWeb {
				base.workspaces = append(base.workspaces, targetWorkspace{ID: "repo-web", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/acme/web.git")})
			}
			base.links = map[string][]string{"project-payments": tc.links}
			result, err := base.resolver().Resolve(context.Background(), resolveTargetInput{OrganizationID: "org-1", ExplicitProjectID: "project-payments"})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != tc.wantStatus || result.Context.RepositoryWorkspaceID != tc.wantRepo || len(result.Candidates) != tc.wantCount {
				t.Fatalf("result = %#v, want status=%q repo=%q candidates=%d", result, tc.wantStatus, tc.wantRepo, tc.wantCount)
			}
		})
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
	links          map[string][]string
	now            time.Time
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
			if f.links != nil {
				return append([]string(nil), f.links[projectID]...), nil
			}
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
		now: func() time.Time {
			if !f.now.IsZero() {
				return f.now
			}
			return time.Unix(1, 0).UTC()
		},
		cache: f.cache,
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

func removeWorkspace(workspaces []targetWorkspace, id string) []targetWorkspace {
	out := make([]targetWorkspace, 0, len(workspaces))
	for _, workspace := range workspaces {
		if workspace.ID != id {
			out = append(out, workspace)
		}
	}
	return out
}

var errNoGitEvidence = fmt.Errorf("no git evidence")
