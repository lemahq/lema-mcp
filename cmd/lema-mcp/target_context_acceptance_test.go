package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Parallel local clients must never share a mutable "last workspace". Each
// request carries its own cwd evidence and receives its own immutable receipt.
func TestTargetResolverParallelRepositoryReceiptsRemainIsolated(t *testing.T) {
	type outcome struct {
		name   string
		result resolutionResult
		err    error
	}
	inputs := []struct {
		name, cwd, workspaceID, remote string
	}{
		{"web", "/repos/web", "repo-web", "https://github.com/acme/web.git"},
		{"api", "/repos/api", "repo-api", "https://github.com/acme/api.git"},
	}
	workspaces := []targetWorkspace{
		{ID: "repo-web", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/acme/web.git")},
		{ID: "repo-api", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/acme/api.git")},
	}
	resolver := &targetResolver{
		fetchWorkspaces: func(context.Context, resolveTargetInput) ([]targetWorkspace, error) {
			return workspaces, nil
		},
		fetchLinks: func(context.Context, resolveTargetInput, string) ([]string, error) {
			return nil, nil
		},
		readGit: func(_ context.Context, cwd string) (gitTargetEvidence, error) {
			for _, input := range inputs {
				if input.cwd == cwd {
					return gitTargetEvidence{RemoteURL: input.remote, Root: input.cwd}, nil
				}
			}
			return gitTargetEvidence{}, errNoGitEvidence
		},
		now:   func() time.Time { return time.Unix(1, 0).UTC() },
		cache: newTargetResolutionCache(64),
	}
	provider := newObservedTargetProvider(newBoundHostedTargetProvider(
		resolver, "https://api.example", "parallel-process-token",
	))
	results := make(chan outcome, len(inputs))
	var wg sync.WaitGroup
	for _, input := range inputs {
		input := input
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := provider.Resolve(context.Background(), resolveTargetInput{
				APIURL: "https://spoofed.example", CredentialFingerprint: "spoofed", OrganizationID: "org-1", CWD: input.cwd,
			})
			results <- outcome{name: input.name, result: result, err: err}
		}()
	}
	wg.Wait()
	close(results)

	seen := map[string]targetContext{}
	for outcome := range results {
		if outcome.err != nil || outcome.result.Status != resolutionResolved {
			t.Fatalf("%s resolution = %#v, %v", outcome.name, outcome.result, outcome.err)
		}
		seen[outcome.name] = outcome.result.Context
	}
	if seen["web"].RepositoryWorkspaceID != "repo-web" || seen["api"].RepositoryWorkspaceID != "repo-api" {
		t.Fatalf("parallel receipts crossed: web=%#v api=%#v", seen["web"], seen["api"])
	}
	if seen["web"].Repository.Canonical == seen["api"].Repository.Canonical {
		t.Fatalf("parallel repositories collapsed to one canonical identity: %#v", seen)
	}
}

// Credential fingerprints partition discovery/cache state even when two users
// are authorized for the same Repository and Project.
func TestTargetResolverCredentialPartitionsRemainIsolated(t *testing.T) {
	const (
		userAToken   = "user-a-token"
		userBToken   = "user-b-token"
		userAProject = "project-payments"
		userBProject = "project-platform"
	)
	repository := targetWorkspace{
		ID: "repo-api", OrganizationID: "org-1", IsRepository: true,
		Repository: mustRepository(t, "https://github.com/acme/api.git"),
	}
	projectsByFingerprint := map[string]string{
		credentialFingerprint(userAToken): userAProject,
		credentialFingerprint(userBToken): userBProject,
	}
	resolver := &targetResolver{
		fetchWorkspaces: func(_ context.Context, input resolveTargetInput) ([]targetWorkspace, error) {
			project, ok := projectsByFingerprint[input.CredentialFingerprint]
			if !ok {
				return nil, nil
			}
			return []targetWorkspace{repository, {ID: project, OrganizationID: "org-1"}}, nil
		},
		fetchLinks: func(_ context.Context, input resolveTargetInput, project string) ([]string, error) {
			if projectsByFingerprint[input.CredentialFingerprint] == project {
				return []string{"repo-api"}, nil
			}
			return nil, nil
		},
		readGit: func(context.Context, string) (gitTargetEvidence, error) {
			return gitTargetEvidence{RemoteURL: "https://github.com/acme/api.git", Root: "/repo"}, nil
		},
		now:   func() time.Time { return time.Unix(1, 0).UTC() },
		cache: newTargetResolutionCache(64),
	}
	providers := map[string]targetProvider{
		"user-a": newObservedTargetProvider(newBoundHostedTargetProvider(resolver, "https://api.example", userAToken)),
		"user-b": newObservedTargetProvider(newBoundHostedTargetProvider(resolver, "https://api.example", userBToken)),
	}
	wantProjects := map[string]string{"user-a": userAProject, "user-b": userBProject}
	type outcome struct {
		user   string
		result resolutionResult
		err    error
	}
	results := make(chan outcome, len(providers))
	var wg sync.WaitGroup
	for user, provider := range providers {
		user, provider := user, provider
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := provider.Resolve(context.Background(), resolveTargetInput{
				APIURL: "https://spoofed.example", CredentialFingerprint: "spoofed", OrganizationID: "org-1", CWD: "/repo",
			})
			results <- outcome{user: user, result: result, err: err}
		}()
	}
	wg.Wait()
	close(results)
	for outcome := range results {
		if outcome.err != nil || outcome.result.Status != resolutionResolved {
			t.Fatalf("%s resolution = %#v, %v", outcome.user, outcome.result, outcome.err)
		}
		if got := outcome.result.Context.ProjectWorkspaceID; got != wantProjects[outcome.user] {
			t.Fatalf("%s crossed credential partitions: project=%q want=%q", outcome.user, got, wantProjects[outcome.user])
		}
		if outcome.result.Context.RepositoryWorkspaceID != "repo-api" {
			t.Fatalf("%s received wrong Repository receipt: %#v", outcome.user, outcome.result.Context)
		}
	}
	if got := resolver.cache.len(); got != 2 {
		t.Fatalf("two bound credential providers produced %d shared-cache entries, want 2", got)
	}
}

func TestTargetResolverAuthoritativeRenameRetainsRepositoryWorkspaceIdentity(t *testing.T) {
	base := resolverFixture(t)
	resolver := base.resolver()
	input := resolveTargetInput{
		APIURL: "https://api.example", CredentialFingerprint: "cred-a", OrganizationID: "org-1", CWD: "/repo",
	}
	before, err := resolver.Resolve(context.Background(), input)
	if err != nil || before.Status != resolutionResolved || before.Context.RepositoryWorkspaceID != "repo-api" {
		t.Fatalf("pre-rename resolution = %#v, %v", before, err)
	}
	workspaceCallsBeforeRename := base.workspaceCalls
	base.workspaces[0].Repository = mustRepository(t, "https://github.com/acme/billing-api.git")
	base.git = gitTargetEvidence{RemoteURL: "https://github.com/acme/billing-api.git", Root: "/repo"}
	result, err := resolver.Resolve(context.Background(), input)
	if err != nil || result.Status != resolutionResolved {
		t.Fatalf("renamed repository resolution = %#v, %v", result, err)
	}
	if result.Context.RepositoryWorkspaceID != "repo-api" {
		t.Fatalf("authoritative URL update changed stable workspace identity: %#v", result.Context)
	}
	if result.Context.Repository.Canonical != "git:github.com/acme/billing-api" {
		t.Fatalf("receipt retained stale canonical repository after rename: %#v", result.Context)
	}
	if base.workspaceCalls <= workspaceCallsBeforeRename {
		t.Fatalf("rename reused the old cached authority without revalidation: before=%d after=%d", workspaceCallsBeforeRename, base.workspaceCalls)
	}
}

func TestTargetResolverOrganizationTransferRejectsStaleReceipt(t *testing.T) {
	base := resolverFixture(t)
	resolver := base.resolver()
	oldInput := resolveTargetInput{
		APIURL: "https://api.example", CredentialFingerprint: "org-1-credential", OrganizationID: "org-1", CWD: "/repo",
	}
	warm, err := resolver.Resolve(context.Background(), oldInput)
	if err != nil || warm.Status != resolutionResolved {
		t.Fatalf("warm pre-transfer receipt = %#v, %v", warm, err)
	}
	base.run = warm.Context
	base.workspaces[0].OrganizationID = "org-new-owner"
	result, err := resolver.Resolve(context.Background(), oldInput)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status == resolutionResolved {
		t.Fatalf("warm pre-transfer cache resolved under the old Organization: %#v", result)
	}
	resumed, err := resolver.Resolve(context.Background(), resolveTargetInput{
		APIURL: "https://api.example", CredentialFingerprint: "org-1-credential", OrganizationID: "org-1", RunID: "run-before-transfer",
	})
	if err != nil || resumed.Status == resolutionResolved {
		t.Fatalf("pre-transfer Run receipt survived transfer: %#v, %v", resumed, err)
	}
	fresh, err := resolver.Resolve(context.Background(), resolveTargetInput{
		APIURL: "https://api.example", CredentialFingerprint: "org-2-credential", OrganizationID: "org-new-owner", CWD: "/repo",
	})
	if err != nil || fresh.Status != resolutionResolved || fresh.Context.RepositoryWorkspaceID != "repo-api" {
		t.Fatalf("new Organization credential did not resolve transferred Repository fresh: %#v, %v", fresh, err)
	}
}

func TestTargetResolverForkOriginRemainsDistinctFromUpstream(t *testing.T) {
	root := t.TempDir()
	gitHere(t, root, "init", "-q")
	gitHere(t, root, "remote", "add", "origin", "https://github.com/contributor/api.git")
	gitHere(t, root, "remote", "add", "upstream", "https://github.com/acme/api.git")
	evidence, err := readContextGitEvidence(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.RemoteURL != "https://github.com/contributor/api.git" || evidence.Ambiguous {
		t.Fatalf("fork evidence selected upstream or became ambiguous: %#v", evidence)
	}
	base := resolverFixture(t)
	base.workspaces = []targetWorkspace{
		{ID: "repo-fork", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/contributor/api.git")},
		{ID: "repo-upstream", OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/acme/api.git")},
	}
	base.readGit = readContextGitEvidence
	result, err := base.resolver().Resolve(context.Background(), resolveTargetInput{OrganizationID: "org-1", CWD: root})
	if err != nil || result.Status != resolutionResolved {
		t.Fatalf("fork resolution = %#v, %v", result, err)
	}
	if result.Context.RepositoryWorkspaceID != "repo-fork" || result.Context.Repository.Canonical != "git:github.com/contributor/api" {
		t.Fatalf("fork collapsed into upstream: %#v", result.Context)
	}
}

func TestStateBriefLegacyUUIDPinRoutesAuthoritativelyAndRedactsSuffix(t *testing.T) {
	const (
		projectUUID = "11111111-2222-3333-4444-555555555555"
		repoUUID    = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)
	srv, cap := newBriefTestServerForProject(t, projectUUID, "", http.StatusOK)
	defer srv.Close()
	base := resolverFixture(t)
	base.workspaces = []targetWorkspace{
		{ID: projectUUID, OrganizationID: "org-1"},
		{ID: repoUUID, OrganizationID: "org-1", IsRepository: true, Repository: mustRepository(t, "https://github.com/acme/api.git")},
	}
	base.links = map[string][]string{projectUUID: {repoUUID}}
	base.git = gitTargetEvidence{RemoteURL: "https://github.com/acme/wrong.git", Root: "/private/wrong"}
	runtime := installStateBriefRuntime(t, base.resolver(), srv, resolveTargetInput{
		ExplicitWorkspaceID: repoUUID, CWD: "/private/operator/api",
	})
	_, out, err := getStateBrief(context.Background(), nil, stateBriefInput{Run: briefRunID})
	if err != nil {
		t.Fatal(err)
	}
	if base.gitCalls != 0 || strings.Join(cap.paths, ",") != "/workspaces/"+projectUUID+"/brief" {
		t.Fatalf("legacy UUID pin was not authoritative: paths=%v gitCalls=%d", cap.paths, base.gitCalls)
	}
	for _, want := range []string{"resolved by explicit", "project UUID ending …55555555", "repository UUID ending …eeeeeeee"} {
		if !strings.Contains(out.Note, want) {
			t.Fatalf("legacy UUID diagnostic missing %q: %q", want, out.Note)
		}
	}
	for _, secret := range []string{projectUUID, repoUUID, runtime.apiURL, runtime.token, runtime.targetInput.CWD} {
		if strings.Contains(out.Note, secret) {
			t.Fatalf("legacy UUID diagnostic leaked %q: %q", secret, out.Note)
		}
	}
}

func TestTargetResolverMonorepoSubdirectoriesShareOneRepositoryReceipt(t *testing.T) {
	base := resolverFixture(t)
	base.readGit = func(_ context.Context, cwd string) (gitTargetEvidence, error) {
		return gitTargetEvidence{RemoteURL: "https://github.com/acme/monorepo.git", Root: "/repos/monorepo"}, nil
	}
	base.workspaces = []targetWorkspace{{
		ID: "repo-monorepo", OrganizationID: "org-1", IsRepository: true,
		Repository: mustRepository(t, "https://github.com/acme/monorepo.git"),
	}}
	resolver := base.resolver()
	var workspaceID string
	for _, cwd := range []string{"/repos/monorepo/apps/web", "/repos/monorepo/services/api"} {
		result, err := resolver.Resolve(context.Background(), resolveTargetInput{OrganizationID: "org-1", CWD: cwd})
		if err != nil || result.Status != resolutionResolved {
			t.Fatalf("monorepo cwd %s resolution = %#v, %v", cwd, result, err)
		}
		if workspaceID == "" {
			workspaceID = result.Context.RepositoryWorkspaceID
		}
		if result.Context.RepositoryWorkspaceID != workspaceID || result.Context.ProjectWorkspaceID != workspaceID {
			t.Fatalf("monorepo subdirectory minted a path subcontext: cwd=%s receipt=%#v", cwd, result.Context)
		}
	}
}
