package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// resolutionStatus keeps resolution failures typed so callers cannot turn an
// uncertainty or authorization failure into an unscoped request.
type resolutionStatus string

const (
	resolutionResolved   resolutionStatus = "resolved"
	resolutionUnresolved resolutionStatus = "unresolved"
	resolutionAmbiguous  resolutionStatus = "ambiguous"
	resolutionForbidden  resolutionStatus = "forbidden"
	resolutionStale      resolutionStatus = "stale"
)

// repositoryIdentity is stable across local clones and deliberately includes
// the Git host. Local paths are evidence, not cross-machine identity.
type repositoryIdentity struct {
	Host      string
	Owner     string
	Name      string
	Canonical string
}

type resolutionEvidence struct {
	Kind  string
	Value string
}

// targetContext is the immutable receipt passed to later operations. This
// first slice only constructs it; production operations do not use it yet.
type targetContext struct {
	OrganizationID                string
	ProjectWorkspaceID            string
	RepositoryWorkspaceID         string
	VisibleRepositoryWorkspaceIDs []string
	Repository                    repositoryIdentity
	ResolvedBy                    string
	Evidence                      []resolutionEvidence
	ResolvedAt                    time.Time
}

type resolutionResult struct {
	Status     resolutionStatus
	Context    targetContext
	Candidates []targetContext
	Reason     string
}

// targetProvider is the process-scoped resolution seam for hosted operations.
// It deliberately returns a receipt for each call rather than retaining a
// mutable active target, because one MCP process can serve multiple checkout
// contexts concurrently.
type targetProvider interface {
	Resolve(context.Context, resolveTargetInput) (resolutionResult, error)
}

// processTargetProvider is constructed once after hosted credentials load.
// Future operation adapters receive it as a dependency; it never contains an
// operation-specific target.
var processTargetProvider targetProvider

// newHostedTargetProvider builds the real process provider from credentials
// already loaded by main. The operation gate never rereads credentials.
func newHostedTargetProvider(client *http.Client, apiURL, token string) targetProvider {
	resolver := newHostedTargetResolver(client, apiURL, token)
	resolver.readGit = readContextGitEvidence
	return newBoundHostedTargetProvider(resolver, apiURL, token)
}

// boundHostedTargetProvider owns the credential-derived cache identity. An
// operation supplies target evidence and explicit overrides, but cannot spoof
// the API or credential partition used by the shared resolver cache.
type boundHostedTargetProvider struct {
	resolver              targetProvider
	apiURL                string
	credentialFingerprint string
}

func newBoundHostedTargetProvider(resolver targetProvider, apiURL, token string) targetProvider {
	return &boundHostedTargetProvider{
		resolver:              resolver,
		apiURL:                apiURL,
		credentialFingerprint: credentialFingerprint(token),
	}
}

func (p *boundHostedTargetProvider) Resolve(ctx context.Context, input resolveTargetInput) (resolutionResult, error) {
	input.APIURL = p.apiURL
	input.CredentialFingerprint = p.credentialFingerprint
	return p.resolver.Resolve(ctx, input)
}

// withResolvedTarget is the single generic operation gate. An operation only
// receives a targetContext by value after the provider has returned a resolved
// result. cloneContext also copies every slice-backed receipt field so an
// operation cannot mutate provider-owned or cached state.
func withResolvedTarget[T any](ctx context.Context, provider targetProvider, input resolveTargetInput, operation func(context.Context, targetContext) (T, error)) (T, error) {
	var zero T
	if provider == nil {
		return zero, &targetResolutionError{status: resolutionUnresolved, rung: "target_provider"}
	}
	result, err := provider.Resolve(ctx, input)
	if err != nil {
		return zero, redactedTargetResolutionError(err)
	}
	if result.Status != resolutionResolved {
		return zero, &targetResolutionError{status: targetGateStatus(result.Status), rung: "target_provider"}
	}
	if !validResolvedTargetContext(result.Context) || operation == nil {
		return zero, &targetResolutionError{status: resolutionUnresolved, rung: "target_provider"}
	}
	return operation(ctx, cloneContext(result.Context))
}

func validResolvedTargetContext(receipt targetContext) bool {
	if strings.TrimSpace(receipt.OrganizationID) == "" ||
		strings.TrimSpace(receipt.ProjectWorkspaceID) == "" ||
		strings.TrimSpace(receipt.RepositoryWorkspaceID) == "" ||
		strings.TrimSpace(receipt.Repository.Canonical) == "" ||
		strings.TrimSpace(receipt.ResolvedBy) == "" ||
		receipt.ResolvedAt.IsZero() || len(receipt.Evidence) == 0 {
		return false
	}
	primaryVisible := false
	for _, workspaceID := range receipt.VisibleRepositoryWorkspaceIDs {
		if workspaceID == receipt.RepositoryWorkspaceID {
			primaryVisible = true
			break
		}
	}
	if !primaryVisible {
		return false
	}
	for _, evidence := range receipt.Evidence {
		if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Value) == "" {
			return false
		}
	}
	return true
}

func targetGateStatus(status resolutionStatus) resolutionStatus {
	switch status {
	case resolutionUnresolved, resolutionAmbiguous, resolutionForbidden, resolutionStale:
		return status
	default:
		return resolutionUnresolved
	}
}

// redactedTargetResolutionError preserves only the typed status and routing
// rung. Resolver transport errors can contain endpoints, credentials, or local
// paths, so they must not cross the operation boundary.
func redactedTargetResolutionError(err error) *targetResolutionError {
	return &targetResolutionError{
		status: targetGateStatus(targetResolutionStatusFromError(err)),
		rung:   targetResolutionRungFromError(err),
	}
}

type resolveTargetInput struct {
	APIURL                string
	CredentialFingerprint string
	OrganizationID        string
	ExplicitWorkspaceID   string
	ExplicitProjectID     string
	ExplicitRepositoryID  string
	ExplicitRepository    repositoryIdentity
	RunID                 string
	CWD                   string
	LocalAssociation      *targetContext
	PersistedAssociation  bool
}

// targetWorkspace is the pure representation supplied by a future adapter for
// GET /workspaces. It intentionally does not perform transport itself.
type targetWorkspace struct {
	ID             string
	OrganizationID string
	IsRepository   bool
	Archived       bool
	Repository     repositoryIdentity
}

type gitTargetEvidence struct {
	RemoteURL  string
	Repository repositoryIdentity
	Root       string
	Ambiguous  bool
}

// targetResolver has only injected seams. It is pure relative to its inputs:
// adapters own HTTP, Git execution, credentials, and production routing.
type targetResolver struct {
	fetchWorkspaces func(context.Context, resolveTargetInput) ([]targetWorkspace, error)
	fetchLinks      func(context.Context, resolveTargetInput, string) ([]string, error)
	resumeRun       func(context.Context, resolveTargetInput, string) (resolutionResult, error)
	readGit         func(context.Context, string) (gitTargetEvidence, error)
	now             func() time.Time
	cache           *targetResolutionCache
}

const targetCacheTTL = time.Minute

func (r *targetResolver) Resolve(ctx context.Context, in resolveTargetInput) (resolutionResult, error) {
	if hasExplicitTarget(in) {
		return r.resolveExplicit(ctx, in)
	}
	if in.RunID != "" && r.resumeRun != nil {
		res, err := r.resumeRun(ctx, in, in.RunID)
		if err != nil || res.Status != resolutionUnresolved {
			if err != nil || res.Status != resolutionResolved {
				return res, err
			}
			return r.validateContext(ctx, in, res.Context, "run")
		}
	}

	var git gitTargetEvidence
	var observedGit repositoryIdentity
	if in.CWD != "" && r.readGit != nil {
		if read, err := r.readGit(ctx, in.CWD); err == nil {
			git = read
			if git.Ambiguous {
				return resolutionResult{Status: resolutionStale, Reason: "current Git remotes do not identify one canonical repository"}, nil
			}
			if git.Repository.Canonical == "" {
				git.Repository, _ = repositoryIdentityFromRemote(git.RemoteURL)
			}
			if git.Repository.Canonical != "" {
				observedGit = git.Repository
				key := r.cacheKey(in, git.Repository)
				identity := git.Repository
				if key != "" {
					if cached, ok := r.cache.get(key, r.timeNow()); ok {
						// Cache only canonical repository discovery. Project selection
						// always comes from current workspace/link authority below.
						identity = cached
					}
				}
				res, err := r.resolveRepository(ctx, in, identity, "canonical_git")
				if err == nil && in.PersistedAssociation && in.LocalAssociation != nil && (res.Status == resolutionResolved || res.Status == resolutionAmbiguous) {
					validated, validateErr := r.validateContext(ctx, in, *in.LocalAssociation, "local_association")
					if validateErr != nil || validated.Status != resolutionResolved {
						return validated, validateErr
					}
					if res.Status == resolutionAmbiguous {
						return validated, nil
					}
					if validated.Context.ProjectWorkspaceID != res.Context.ProjectWorkspaceID || validated.Context.RepositoryWorkspaceID != res.Context.RepositoryWorkspaceID {
						return resolutionResult{Status: resolutionStale, Reason: "local association no longer matches the canonical repository target"}, nil
					}
				}
				if err != nil || res.Status != resolutionUnresolved {
					if err == nil && res.Status == resolutionResolved {
						res.Context.Evidence = append(res.Context.Evidence, gitEvidence(in, git)...)
						if key != "" {
							r.cache.put(key, identity, r.timeNow())
						}
					}
					return res, err
				}
			}
		}
	}
	if in.LocalAssociation != nil {
		if observedGit.Canonical != "" && in.LocalAssociation.Repository.Canonical != observedGit.Canonical {
			return resolutionResult{Status: resolutionStale, Reason: "local association belongs to a different repository than current Git evidence"}, nil
		}
		return r.validateContext(ctx, in, *in.LocalAssociation, "local_association")
	}
	return resolutionResult{Status: resolutionUnresolved, Reason: "no validated explicit target, run, Git repository, or local association"}, nil
}

func hasExplicitTarget(in resolveTargetInput) bool {
	return in.ExplicitWorkspaceID != "" || in.ExplicitProjectID != "" || in.ExplicitRepositoryID != "" || in.ExplicitRepository.Canonical != ""
}

func (r *targetResolver) resolveExplicit(ctx context.Context, in resolveTargetInput) (resolutionResult, error) {
	workspaces, err := r.workspaces(ctx, in)
	if err != nil {
		return resolutionResult{}, err
	}
	if in.ExplicitWorkspaceID != "" {
		for _, workspace := range workspaces {
			if workspace.ID != in.ExplicitWorkspaceID {
				continue
			}
			if workspace.Archived {
				return resolutionResult{Status: resolutionStale, Reason: "explicit workspace is archived"}, nil
			}
			if !r.workspaceInOrganization(in, workspace) {
				return resolutionResult{Status: resolutionStale, Reason: "explicit workspace is not visible in the selected organization"}, nil
			}
			if !workspace.IsRepository {
				return resolutionResult{Status: resolutionStale, Reason: "explicit workspace is not a repository target"}, nil
			}
			return r.resolveRepositoryWithWorkspaces(ctx, in, workspace, workspaces, in.ExplicitProjectID, "explicit")
		}
		return resolutionResult{Status: resolutionStale, Reason: "explicit workspace is no longer visible"}, nil
	}
	if in.ExplicitRepositoryID != "" {
		for _, workspace := range workspaces {
			if workspace.ID != in.ExplicitRepositoryID {
				continue
			}
			if workspace.Archived || !workspace.IsRepository || !r.workspaceInOrganization(in, workspace) {
				return resolutionResult{Status: resolutionStale, Reason: "explicit repository is not an active visible repository"}, nil
			}
			return r.resolveRepositoryWithWorkspaces(ctx, in, workspace, workspaces, in.ExplicitProjectID, "explicit")
		}
		return resolutionResult{Status: resolutionForbidden, Reason: "explicit repository is not visible to this credential"}, nil
	}
	if in.ExplicitRepository.Canonical == "" && in.ExplicitProjectID != "" {
		return r.resolveExplicitProject(ctx, in, workspaces)
	}
	for _, workspace := range workspaces {
		if workspace.IsRepository && workspace.Repository.Canonical == in.ExplicitRepository.Canonical && !workspace.Archived && r.workspaceInOrganization(in, workspace) {
			return r.resolveRepositoryWithWorkspaces(ctx, in, workspace, workspaces, in.ExplicitProjectID, "explicit")
		}
	}
	return resolutionResult{Status: resolutionForbidden, Reason: "explicit repository is not visible to this credential"}, nil
}

func (r *targetResolver) resolveExplicitProject(ctx context.Context, in resolveTargetInput, workspaces []targetWorkspace) (resolutionResult, error) {
	if r.fetchLinks == nil {
		return resolutionResult{Status: resolutionUnresolved, Reason: "project link authority is unavailable"}, nil
	}
	var project *targetWorkspace
	for i := range workspaces {
		workspace := &workspaces[i]
		if workspace.ID == in.ExplicitProjectID && !workspace.IsRepository && !workspace.Archived && r.workspaceInOrganization(in, *workspace) {
			project = workspace
			break
		}
	}
	if project == nil {
		return resolutionResult{Status: resolutionStale, Reason: "explicit project is not visible or active"}, nil
	}
	visible, err := r.visibleRepositories(ctx, in, *project, workspaces)
	if err != nil {
		return resolutionResult{}, err
	}
	byID := make(map[string]targetWorkspace, len(workspaces))
	for _, workspace := range workspaces {
		if workspace.IsRepository && !workspace.Archived && r.workspaceInOrganization(in, workspace) && workspace.OrganizationID == project.OrganizationID {
			byID[workspace.ID] = workspace
		}
	}
	switch len(visible) {
	case 0:
		return resolutionResult{Status: resolutionUnresolved, Reason: "explicit project has no visible linked repository"}, nil
	case 1:
		repository := byID[visible[0]]
		return resolutionResult{Status: resolutionResolved, Context: r.contextFor(in, *project, repository, visible, "explicit")}, nil
	default:
		candidates := make([]targetContext, 0, len(visible))
		for _, id := range visible {
			candidates = append(candidates, r.contextFor(in, *project, byID[id], visible, ""))
		}
		return resolutionResult{Status: resolutionAmbiguous, Candidates: candidates, Reason: "explicit project has multiple visible linked repositories"}, nil
	}
}

func (r *targetResolver) resolveRepository(ctx context.Context, in resolveTargetInput, identity repositoryIdentity, resolvedBy string) (resolutionResult, error) {
	workspaces, err := r.workspaces(ctx, in)
	if err != nil {
		return resolutionResult{}, err
	}
	for _, workspace := range workspaces {
		if workspace.IsRepository && !workspace.Archived && r.workspaceInOrganization(in, workspace) && workspace.Repository.Canonical == identity.Canonical {
			return r.resolveRepositoryWithWorkspaces(ctx, in, workspace, workspaces, "", resolvedBy)
		}
	}
	return resolutionResult{Status: resolutionUnresolved, Reason: "Git repository is not registered or visible"}, nil
}

func (r *targetResolver) resolveRepositoryWithWorkspaces(ctx context.Context, in resolveTargetInput, repository targetWorkspace, workspaces []targetWorkspace, explicitProjectID, resolvedBy string) (resolutionResult, error) {
	if r.fetchLinks == nil {
		return resolutionResult{Status: resolutionUnresolved, Reason: "project link authority is unavailable"}, nil
	}
	parents, err := r.parents(ctx, in, repository, workspaces)
	if err != nil {
		return resolutionResult{}, err
	}
	if explicitProjectID != "" {
		for _, parent := range parents {
			if parent.ID == explicitProjectID {
				visible, err := r.visibleRepositories(ctx, in, parent, workspaces)
				if err != nil {
					return resolutionResult{}, err
				}
				return resolutionResult{Status: resolutionResolved, Context: r.contextFor(in, parent, repository, visible, resolvedBy)}, nil
			}
		}
		return resolutionResult{Status: resolutionStale, Reason: "explicit project does not currently link the repository"}, nil
	}
	switch len(parents) {
	case 0:
		// Existing unlinked repository leaves remain compatible singleton Projects.
		return resolutionResult{Status: resolutionResolved, Context: r.contextFor(in, repository, repository, []string{repository.ID}, resolvedBy)}, nil
	case 1:
		visible, err := r.visibleRepositories(ctx, in, parents[0], workspaces)
		if err != nil {
			return resolutionResult{}, err
		}
		return resolutionResult{Status: resolutionResolved, Context: r.contextFor(in, parents[0], repository, visible, resolvedBy)}, nil
	default:
		candidates := make([]targetContext, 0, len(parents))
		for _, parent := range parents {
			visible, err := r.visibleRepositories(ctx, in, parent, workspaces)
			if err != nil {
				return resolutionResult{}, err
			}
			candidates = append(candidates, r.contextFor(in, parent, repository, visible, ""))
		}
		return resolutionResult{Status: resolutionAmbiguous, Candidates: candidates, Reason: "repository belongs to multiple visible projects"}, nil
	}
}

func (r *targetResolver) validateContext(ctx context.Context, in resolveTargetInput, receipt targetContext, resolvedBy string) (resolutionResult, error) {
	workspaces, err := r.workspaces(ctx, in)
	if err != nil {
		return resolutionResult{}, err
	}
	var repository, project *targetWorkspace
	for i := range workspaces {
		workspace := &workspaces[i]
		if workspace.ID == receipt.RepositoryWorkspaceID && workspace.IsRepository && !workspace.Archived && r.workspaceInOrganization(in, *workspace) && workspace.Repository.Canonical == receipt.Repository.Canonical {
			repository = workspace
		}
		if workspace.ID == receipt.ProjectWorkspaceID && !workspace.IsRepository && !workspace.Archived && r.workspaceInOrganization(in, *workspace) {
			project = workspace
		}
	}
	if repository == nil {
		return resolutionResult{Status: resolutionStale, Reason: "stored target is no longer visible"}, nil
	}
	if receipt.ProjectWorkspaceID == receipt.RepositoryWorkspaceID {
		if r.fetchLinks == nil {
			return resolutionResult{Status: resolutionUnresolved, Reason: "project link authority is unavailable"}, nil
		}
		parents, err := r.parents(ctx, in, *repository, workspaces)
		if err != nil {
			return resolutionResult{}, err
		}
		if len(parents) != 0 {
			return resolutionResult{Status: resolutionStale, Reason: "stored singleton repository is now linked to a project"}, nil
		}
		return resolutionResult{Status: resolutionResolved, Context: r.contextFor(in, *repository, *repository, []string{repository.ID}, resolvedBy)}, nil
	}
	if project == nil || project.OrganizationID != repository.OrganizationID {
		return resolutionResult{Status: resolutionStale, Reason: "stored project is no longer an active project in the repository organization"}, nil
	}
	if receipt.ProjectWorkspaceID != receipt.RepositoryWorkspaceID {
		linked, err := r.linked(ctx, in, receipt.ProjectWorkspaceID, receipt.RepositoryWorkspaceID)
		if err != nil {
			return resolutionResult{}, err
		}
		if !linked {
			return resolutionResult{Status: resolutionStale, Reason: "stored project no longer links the repository"}, nil
		}
	}
	visible, err := r.visibleRepositories(ctx, in, *project, workspaces)
	if err != nil {
		return resolutionResult{}, err
	}
	context := r.contextFor(in, *project, *repository, visible, resolvedBy)
	for _, evidence := range receipt.Evidence {
		if evidence.Kind == "local_root_hash" {
			context.Evidence = append(context.Evidence, evidence)
		}
	}
	context.Evidence = append(context.Evidence, resolutionEvidence{Kind: "validated_receipt", Value: repository.Repository.Canonical})
	return resolutionResult{Status: resolutionResolved, Context: context}, nil
}

func (r *targetResolver) parents(ctx context.Context, in resolveTargetInput, repository targetWorkspace, workspaces []targetWorkspace) ([]targetWorkspace, error) {
	var out []targetWorkspace
	for _, workspace := range workspaces {
		if workspace.IsRepository || workspace.Archived || !r.workspaceInOrganization(in, workspace) || workspace.OrganizationID != repository.OrganizationID {
			continue
		}
		linked, err := r.linked(ctx, in, workspace.ID, repository.ID)
		if err != nil {
			return nil, err
		}
		if linked {
			out = append(out, workspace)
		}
	}
	return out, nil
}

func (r *targetResolver) linked(ctx context.Context, in resolveTargetInput, projectID, repositoryID string) (bool, error) {
	if r.fetchLinks == nil {
		return false, nil
	}
	ids, err := r.fetchLinks(ctx, in, projectID)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		if id == repositoryID {
			return true, nil
		}
	}
	return false, nil
}

func (r *targetResolver) visibleRepositories(ctx context.Context, in resolveTargetInput, project targetWorkspace, workspaces []targetWorkspace) ([]string, error) {
	if r.fetchLinks == nil {
		return nil, nil
	}
	linkedIDs, err := r.fetchLinks(ctx, in, project.ID)
	if err != nil {
		return nil, err
	}
	linked := make(map[string]bool, len(linkedIDs))
	for _, id := range linkedIDs {
		linked[id] = true
	}
	visible := make([]string, 0, len(linked))
	for _, workspace := range workspaces {
		if workspace.IsRepository && !workspace.Archived && r.workspaceInOrganization(in, workspace) && workspace.OrganizationID == project.OrganizationID && linked[workspace.ID] {
			visible = append(visible, workspace.ID)
		}
	}
	return visible, nil
}

func (r *targetResolver) contextFor(in resolveTargetInput, project, repository targetWorkspace, visible []string, resolvedBy string) targetContext {
	return targetContext{OrganizationID: repository.OrganizationID, ProjectWorkspaceID: project.ID, RepositoryWorkspaceID: repository.ID, VisibleRepositoryWorkspaceIDs: append([]string(nil), visible...), Repository: repository.Repository, ResolvedBy: resolvedBy, Evidence: []resolutionEvidence{{Kind: "canonical_remote", Value: repository.Repository.Canonical}}, ResolvedAt: r.timeNow()}
}

func (r *targetResolver) workspaceInOrganization(in resolveTargetInput, workspace targetWorkspace) bool {
	return workspace.OrganizationID != "" && (in.OrganizationID == "" || workspace.OrganizationID == in.OrganizationID)
}

func (r *targetResolver) workspaces(ctx context.Context, in resolveTargetInput) ([]targetWorkspace, error) {
	if r.fetchWorkspaces == nil {
		return nil, nil
	}
	return r.fetchWorkspaces(ctx, in)
}

func (r *targetResolver) timeNow() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now().UTC()
}

func gitEvidence(in resolveTargetInput, git gitTargetEvidence) []resolutionEvidence {
	path := in.CWD
	if path == "" {
		path = git.Root
	}
	return []resolutionEvidence{{Kind: "cwd_path_hash", Value: pathHash(path)}, {Kind: "git_root_path_hash", Value: pathHash(git.Root)}}
}

func pathHash(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

func repositoryIdentityFromRemote(remote string) (repositoryIdentity, bool) {
	s := strings.TrimSpace(remote)
	if s == "" {
		return repositoryIdentity{}, false
	}
	var host, path string
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return repositoryIdentity{}, false
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "http" && scheme != "https" && scheme != "ssh" {
			return repositoryIdentity{}, false
		}
		host = strings.ToLower(u.Hostname())
		port := u.Port()
		if port != "" && !isDefaultGitPort(scheme, port) {
			host += ":" + port
		}
		path = u.Path
	} else if colon := strings.Index(s, ":"); colon > 0 { // scp-style git@host:owner/repo
		host, path = s[:colon], s[colon+1:]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		path = strings.SplitN(path, "?", 2)[0]
		path = strings.SplitN(path, "#", 2)[0]
	} else {
		return repositoryIdentity{}, false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	segments := make([]string, 0, 2)
	for _, segment := range strings.Split(strings.Trim(path, "/"), "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	if host == "" || len(segments) < 2 {
		return repositoryIdentity{}, false
	}
	owner := strings.ToLower(segments[len(segments)-2])
	name := strings.TrimSuffix(strings.ToLower(segments[len(segments)-1]), ".git")
	if owner == "" || name == "" {
		return repositoryIdentity{}, false
	}
	return repositoryIdentity{Host: host, Owner: owner, Name: name, Canonical: "git:" + host + "/" + owner + "/" + name}, true
}

func isDefaultGitPort(scheme, port string) bool {
	p, err := strconv.Atoi(port)
	if err != nil {
		return false
	}
	return (scheme == "http" && p == 80) || (scheme == "https" && p == 443) || (scheme == "ssh" && p == 22)
}

func (r *targetResolver) cacheKey(in resolveTargetInput, repository repositoryIdentity) string {
	if in.CredentialFingerprint == "" {
		return ""
	}
	return strings.Join([]string{in.APIURL, in.CredentialFingerprint, in.OrganizationID, repository.Canonical, in.ExplicitProjectID, pathHash(in.CWD)}, "\x00")
}

type targetResolutionCache struct {
	mu       sync.Mutex
	capacity int
	values   map[string]targetCacheEntry
	order    []string
}

func newTargetResolutionCache(capacity int) *targetResolutionCache {
	if capacity < 1 {
		capacity = 1
	}
	return &targetResolutionCache{capacity: capacity, values: make(map[string]targetCacheEntry)}
}

type targetCacheEntry struct {
	repository repositoryIdentity
	expires    time.Time
}

func (c *targetResolutionCache) get(key string, now time.Time) (repositoryIdentity, bool) {
	if c == nil {
		return repositoryIdentity{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	value, ok := c.values[key]
	if !ok {
		return repositoryIdentity{}, false
	}
	if !now.Before(value.expires) {
		delete(c.values, key)
		for i, candidate := range c.order {
			if candidate == key {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		return repositoryIdentity{}, false
	}
	return value.repository, true
}

func (c *targetResolutionCache) put(key string, value repositoryIdentity, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.values[key]; !exists {
		c.order = append(c.order, key)
	}
	c.values[key] = targetCacheEntry{repository: value, expires: now.Add(targetCacheTTL)}
	for len(c.order) > c.capacity {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.values, oldest)
	}
}

func (c *targetResolutionCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.values)
}

func cloneContext(in targetContext) targetContext {
	in.VisibleRepositoryWorkspaceIDs = append([]string(nil), in.VisibleRepositoryWorkspaceIDs...)
	in.Evidence = append([]resolutionEvidence(nil), in.Evidence...)
	return in
}
