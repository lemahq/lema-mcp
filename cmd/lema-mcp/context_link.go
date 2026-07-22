package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const contextAssociationSchemaVersion = 1

// contextLinkOptions contains only local boundary adapters. The association is
// never authority: runContextLink obtains it from the same targetResolver used
// by doctor, then stores the resolver's validated receipt in a local file.
type contextLinkOptions struct {
	APIURL  string
	Token   string
	CWD     string
	ReadGit func(context.Context, string) (gitTargetEvidence, error)
	Output  io.Writer
}

type persistedContextAssociation struct {
	SchemaVersion         int    `json:"schema_version"`
	ProjectWorkspaceID    string `json:"project_workspace_id"`
	RepositoryWorkspaceID string `json:"repository_workspace_id"`
	RepositoryCanonical   string `json:"repository_canonical,omitempty"`
	LocalRootHash         string `json:"local_root_hash"`
}

type contextAssociationError struct{ status resolutionStatus }

func (e *contextAssociationError) Error() string { return "context association " + string(e.status) }

func isContextAssociationStale(err error) bool {
	var associationErr *contextAssociationError
	return errors.As(err, &associationErr) && associationErr.status == resolutionStale
}

func runContextCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lema-mcp context link --project ID --repository ID | lema-mcp context unlink")
	}
	switch args[0] {
	case "link":
		flags := flag.NewFlagSet("context link", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		projectID := flags.String("project", "", "project workspace UUID")
		repositoryID := flags.String("repository", "", "repository workspace UUID")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(*projectID) == "" || strings.TrimSpace(*repositoryID) == "" {
			return fmt.Errorf("usage: lema-mcp context link --project ID --repository ID")
		}
		config, err := resolveDoctorConfig()
		if err != nil {
			return fmt.Errorf("context link requires configured hosted credentials")
		}
		return runContextLink(context.Background(), contextLinkOptions{APIURL: config.APIURL, Token: config.Token}, strings.TrimSpace(*projectID), strings.TrimSpace(*repositoryID))
	case "unlink":
		if len(args) != 1 {
			return fmt.Errorf("usage: lema-mcp context unlink")
		}
		return runContextUnlink(contextLinkOptions{})
	default:
		return fmt.Errorf("usage: lema-mcp context link --project ID --repository ID | lema-mcp context unlink")
	}
}

func runContextLink(ctx context.Context, options contextLinkOptions, projectID, repositoryID string) error {
	if strings.TrimSpace(options.APIURL) == "" || strings.TrimSpace(options.Token) == "" {
		return fmt.Errorf("context link requires configured hosted credentials")
	}
	root, git := contextAssociationRoot(ctx, options.CWD, options.ReadGit)
	if git.Ambiguous {
		return &contextAssociationError{status: resolutionStale}
	}
	resolver := newHostedTargetResolver(contextHTTPClient(), options.APIURL, options.Token)
	result, err := resolver.Resolve(ctx, resolveTargetInput{
		APIURL:                options.APIURL,
		CredentialFingerprint: credentialFingerprint(options.Token),
		ExplicitProjectID:     projectID,
		ExplicitRepositoryID:  repositoryID,
	})
	if err != nil {
		return fmt.Errorf("context link unresolved")
	}
	if result.Status != resolutionResolved {
		return &contextAssociationError{status: result.Status}
	}
	if git.Repository.Canonical == "" {
		git.Repository, _ = repositoryIdentityFromRemote(git.RemoteURL)
	}
	if git.Repository.Canonical != "" && git.Repository.Canonical != result.Context.Repository.Canonical {
		return &contextAssociationError{status: resolutionStale}
	}
	if err := writeContextAssociation(root, result.Context); err != nil {
		return err
	}
	if options.Output != nil {
		fmt.Fprintln(options.Output, "context association saved")
	}
	return nil
}

// contextHTTPClient keeps CLI link's HTTP boundary explicit without allowing a
// persisted association to carry a client, token, or API URL.
func contextHTTPClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

func runContextUnlink(options contextLinkOptions) error {
	root, _ := contextAssociationRoot(context.Background(), options.CWD, options.ReadGit)
	rootFS, err := openContextAssociationRoot(root, false)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no context association exists")
		}
		return err
	}
	defer rootFS.Close()
	if _, err := rootFS.Lstat(contextAssociationFile); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no context association exists")
		}
		return contextAssociationUnavailable()
	}
	for n := 0; ; n++ {
		backup := contextAssociationFile + ".bak"
		if n > 0 {
			backup = fmt.Sprintf("%s.%d", backup, n)
		}
		// Claim the backup name before renaming. O_EXCL avoids the usual
		// Lstat→Rename race that could otherwise replace a user's recovery file.
		claim, err := rootFS.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := claim.Close(); err != nil {
			_ = rootFS.Remove(backup)
			return contextAssociationUnavailable()
		}
		if err := rootFS.Rename(contextAssociationFile, backup); err != nil {
			_ = rootFS.Remove(backup)
			return contextAssociationUnavailable()
		}
		return nil
	}
}

func loadContextAssociation(cwd string, readGit func(context.Context, string) (gitTargetEvidence, error)) (targetContext, bool, error) {
	root, git := contextAssociationRoot(context.Background(), cwd, readGit)
	rootFS, err := openContextAssociationRoot(root, false)
	if err != nil {
		if os.IsNotExist(err) {
			return targetContext{}, false, nil
		}
		// A malformed association directory is a saved local-context failure, not
		// an invitation to continue with another resolution rung.
		return targetContext{}, true, err
	}
	defer rootFS.Close()
	data, err := rootFS.ReadFile(contextAssociationFile)
	if err != nil {
		if os.IsNotExist(err) {
			return targetContext{}, false, nil
		}
		return targetContext{}, false, err
	}
	var stored persistedContextAssociation
	if err := json.Unmarshal(data, &stored); err != nil || stored.SchemaVersion != contextAssociationSchemaVersion || stored.ProjectWorkspaceID == "" || stored.RepositoryWorkspaceID == "" || stored.LocalRootHash == "" {
		return targetContext{}, true, &contextAssociationError{status: resolutionStale}
	}
	if stored.LocalRootHash != pathHash(root) {
		return targetContext{}, true, &contextAssociationError{status: resolutionStale}
	}
	if git.Repository.Canonical == "" {
		git.Repository, _ = repositoryIdentityFromRemote(git.RemoteURL)
	}
	if git.Ambiguous || (stored.RepositoryCanonical != "" && git.Repository.Canonical != "" && stored.RepositoryCanonical != git.Repository.Canonical) {
		return targetContext{}, true, &contextAssociationError{status: resolutionStale}
	}
	return targetContext{
		ProjectWorkspaceID:    stored.ProjectWorkspaceID,
		RepositoryWorkspaceID: stored.RepositoryWorkspaceID,
		Repository:            repositoryIdentity{Canonical: stored.RepositoryCanonical},
		Evidence:              []resolutionEvidence{{Kind: "local_root_hash", Value: stored.LocalRootHash}},
	}, true, nil
}

func writeContextAssociation(root string, receipt targetContext) error {
	rootFS, err := openContextAssociationRoot(root, true)
	if err != nil {
		return contextAssociationUnavailable()
	}
	defer rootFS.Close()
	stored := persistedContextAssociation{
		SchemaVersion:         contextAssociationSchemaVersion,
		ProjectWorkspaceID:    receipt.ProjectWorkspaceID,
		RepositoryWorkspaceID: receipt.RepositoryWorkspaceID,
		RepositoryCanonical:   receipt.Repository.Canonical,
		LocalRootHash:         pathHash(root),
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return contextAssociationUnavailable()
	}
	temporaryName, err := contextAssociationTemporaryName()
	if err != nil {
		return contextAssociationUnavailable()
	}
	defer rootFS.Remove(temporaryName)
	temporary, err := rootFS.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return contextAssociationUnavailable()
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return contextAssociationUnavailable()
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return contextAssociationUnavailable()
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return contextAssociationUnavailable()
	}
	if err := temporary.Close(); err != nil {
		return contextAssociationUnavailable()
	}
	if err := rootFS.Rename(temporaryName, contextAssociationFile); err != nil {
		return contextAssociationUnavailable()
	}
	return nil
}

const (
	contextAssociationDirectory = ".lema"
	contextAssociationFile      = ".lema/context.json"
)

func contextAssociationPath(root string) string { return filepath.Join(root, contextAssociationFile) }

func contextAssociationUnavailable() error {
	return &contextAssociationError{status: resolutionStale}
}

// openContextAssociationRoot retains a descriptor for the selected repository
// root. Every subsequent operation is root-relative, so replacing .lema after
// validation cannot redirect a write, rename, or remove outside that root.
func openContextAssociationRoot(root string, create bool) (*os.Root, error) {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, contextAssociationUnavailable()
	}
	info, err := rootFS.Lstat(contextAssociationDirectory)
	if os.IsNotExist(err) && create {
		if err := rootFS.Mkdir(contextAssociationDirectory, 0o700); err != nil && !os.IsExist(err) {
			rootFS.Close()
			return nil, contextAssociationUnavailable()
		}
		info, err = rootFS.Lstat(contextAssociationDirectory)
	}
	if err != nil {
		rootFS.Close()
		if os.IsNotExist(err) {
			return nil, err
		}
		return nil, contextAssociationUnavailable()
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		rootFS.Close()
		return nil, contextAssociationUnavailable()
	}
	return rootFS, nil
}

func contextAssociationTemporaryName() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/.context-%x.tmp", contextAssociationDirectory, random), nil
}

func contextAssociationRoot(ctx context.Context, cwd string, readGit func(context.Context, string) (gitTargetEvidence, error)) (string, gitTargetEvidence) {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if readGit == nil {
		readGit = readContextGitEvidence
	}
	git, _ := readGit(ctx, cwd)
	root := strings.TrimSpace(git.Root)
	if root == "" {
		root = cwd
	}
	return filepath.Clean(root), git
}

func readContextGitEvidence(ctx context.Context, cwd string) (gitTargetEvidence, error) {
	root, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return gitTargetEvidence{}, err
	}
	evidence := gitTargetEvidence{Root: strings.TrimSpace(string(root))}
	remote, ambiguous := selectContextGitRemote(ctx, cwd)
	evidence.RemoteURL, evidence.Ambiguous = remote, ambiguous
	return evidence, nil
}

// selectContextGitRemote follows the stable local ordering from the Target
// Context contract. A checkout with more than one unranked usable remote is
// not treated as a no-remote checkout: ambiguity is explicit and fails closed.
func selectContextGitRemote(ctx context.Context, cwd string) (string, bool) {
	for _, key := range []string{"lema.canonicalRemote", "remote.pushDefault"} {
		if name := gitConfigValue(ctx, cwd, key); name != "" {
			if remote := gitRemoteURLForName(ctx, cwd, name); remote != "" {
				return remote, false
			}
		}
	}
	for _, name := range []string{"origin", "upstream"} {
		if remote := gitRemoteURLForName(ctx, cwd, name); remote != "" {
			return remote, false
		}
	}
	remotesOut, err := exec.CommandContext(ctx, "git", "-C", cwd, "remote").Output()
	if err != nil {
		return "", false
	}
	var candidates []string
	for _, name := range strings.Fields(string(remotesOut)) {
		if remote := gitRemoteURLForName(ctx, cwd, name); remote != "" {
			if _, ok := repositoryIdentityFromRemote(remote); ok {
				candidates = append(candidates, remote)
			}
		}
	}
	if len(candidates) == 1 {
		return candidates[0], false
	}
	return "", len(candidates) > 1
}

func gitConfigValue(ctx context.Context, cwd, key string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitRemoteURLForName(ctx context.Context, cwd, name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	return gitConfigValue(ctx, cwd, "remote."+name+".url")
}
