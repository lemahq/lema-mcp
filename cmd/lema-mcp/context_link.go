package main

import (
	"context"
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
	path := contextAssociationPath(root)
	if _, err := os.Lstat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no context association exists")
		}
		return err
	}
	for n := 0; ; n++ {
		backup := path + ".bak"
		if n > 0 {
			backup = fmt.Sprintf("%s.%d", backup, n)
		}
		// Claim the backup name before renaming. O_EXCL avoids the usual
		// Lstat→Rename race that could otherwise replace a user's recovery file.
		claim, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := claim.Close(); err != nil {
			_ = os.Remove(backup)
			return err
		}
		if err := os.Rename(path, backup); err != nil {
			_ = os.Remove(backup)
			return err
		}
		return nil
	}
}

func loadContextAssociation(cwd string, readGit func(context.Context, string) (gitTargetEvidence, error)) (targetContext, bool, error) {
	root, git := contextAssociationRoot(context.Background(), cwd, readGit)
	data, err := os.ReadFile(contextAssociationPath(root))
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
	if stored.RepositoryCanonical != "" && git.Repository.Canonical != "" && stored.RepositoryCanonical != git.Repository.Canonical {
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
	dir := filepath.Dir(contextAssociationPath(root))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	stored := persistedContextAssociation{
		SchemaVersion:         contextAssociationSchemaVersion,
		ProjectWorkspaceID:    receipt.ProjectWorkspaceID,
		RepositoryWorkspaceID: receipt.RepositoryWorkspaceID,
		RepositoryCanonical:   receipt.Repository.Canonical,
		LocalRootHash:         pathHash(root),
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	path := contextAssociationPath(root)
	temporary, err := os.CreateTemp(dir, ".context-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func contextAssociationPath(root string) string { return filepath.Join(root, ".lema", "context.json") }

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
	remote, err := exec.CommandContext(ctx, "git", "-C", cwd, "config", "--get", "remote.origin.url").Output()
	if err == nil {
		evidence.RemoteURL = strings.TrimSpace(string(remote))
	}
	return evidence, nil
}
