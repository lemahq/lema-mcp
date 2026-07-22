package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// doctorContextOptions keeps the command's process adapters at the boundary.
// Resolution itself remains wholly in targetResolver, so diagnostics and future
// scoped operations cannot drift into separate target-selection algorithms.
type doctorContextOptions struct {
	APIURL  string
	Token   string
	CWD     string
	ReadGit func(context.Context, string) (gitTargetEvidence, error)
	Output  io.Writer
	Client  *http.Client
}

// runDoctorContext resolves the current checkout through the shared typed
// resolver and writes a deliberately small, non-secret trace. It returns an
// error for every non-resolved result; callers must not treat its output as
// permission to make an unscoped request.
func runDoctorContext(ctx context.Context, options doctorContextOptions) error {
	out := options.Output
	if out == nil {
		out = os.Stdout
	}
	if options.CWD == "" {
		options.CWD, _ = os.Getwd()
	}
	if options.ReadGit == nil {
		options.ReadGit = readDoctorGitEvidence
	}

	if strings.TrimSpace(options.APIURL) == "" || strings.TrimSpace(options.Token) == "" {
		fmt.Fprintln(out, "credential     unavailable")
		fmt.Fprintln(out, "result         unresolved")
		fmt.Fprintln(out, "action         configure LEMA_API_URL and LEMA_API_TOKEN")
		return fmt.Errorf("target context unresolved")
	}

	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resolver := newHostedTargetResolver(client, options.APIURL, options.Token)
	resolver.readGit = options.ReadGit
	result, err := resolver.Resolve(ctx, resolveTargetInput{
		APIURL:                options.APIURL,
		CredentialFingerprint: credentialFingerprint(options.Token),
		ExplicitWorkspaceID:   resolveWorkspaceID(),
		CWD:                   options.CWD,
	})
	if err != nil {
		fmt.Fprintln(out, "credential     "+redactedCredentialIdentity(options.Token))
		fmt.Fprintln(out, "result         unresolved")
		fmt.Fprintln(out, "action         verify the hosted API is reachable with this credential")
		return fmt.Errorf("target context unresolved")
	}
	writeDoctorContext(out, options.Token, result)
	if result.Status != resolutionResolved {
		return fmt.Errorf("target context %s", result.Status)
	}
	return nil
}

func runDoctorContextCommand(args []string) error {
	if len(args) != 1 || args[0] != "context" {
		return fmt.Errorf("usage: lema-mcp doctor context")
	}
	apiURL, token, _ := resolveHostedConfig()
	return runDoctorContext(context.Background(), doctorContextOptions{APIURL: apiURL, Token: token})
}

func writeDoctorContext(out io.Writer, token string, result resolutionResult) {
	fmt.Fprintln(out, "credential     "+redactedCredentialIdentity(token))
	if result.Status != resolutionResolved {
		fmt.Fprintln(out, "result         "+string(result.Status))
		fmt.Fprintln(out, "action         "+doctorCorrectiveAction(result.Status))
		return
	}

	context := result.Context
	fmt.Fprintln(out, "organization   "+redactedIDSuffix(context.OrganizationID))
	fmt.Fprintln(out, "repository     "+context.Repository.Canonical)
	fmt.Fprintln(out, "project        UUID ending "+redactedUUIDSuffix(context.ProjectWorkspaceID))
	fmt.Fprintln(out, "repository id  UUID ending "+redactedUUIDSuffix(context.RepositoryWorkspaceID))
	for _, evidence := range context.Evidence {
		switch evidence.Kind {
		case "cwd_path_hash":
			fmt.Fprintln(out, "cwd hash       "+evidence.Value)
		case "git_root_path_hash":
			fmt.Fprintln(out, "git root hash  "+evidence.Value)
		}
	}
	fmt.Fprintln(out, "result         resolved by "+context.ResolvedBy)
}

func redactedCredentialIdentity(token string) string {
	fingerprint := credentialFingerprint(token)
	return "sha256:…" + fingerprint[len(fingerprint)-12:]
}

func redactedIDSuffix(id string) string {
	if len(id) <= 8 {
		return "[redacted]"
	}
	return "…" + id[len(id)-8:]
}

func redactedUUIDSuffix(id string) string {
	return redactedIDSuffix(id)
}

func doctorCorrectiveAction(status resolutionStatus) string {
	switch status {
	case resolutionAmbiguous:
		return "set LEMA_WORKSPACE_ID to the intended visible repository workspace"
	case resolutionForbidden:
		return "switch to a credential authorized for this repository"
	case resolutionStale:
		return "update or remove the stale LEMA_WORKSPACE_ID"
	default:
		return "set LEMA_WORKSPACE_ID to a visible repository workspace"
	}
}

func readDoctorGitEvidence(ctx context.Context, cwd string) (gitTargetEvidence, error) {
	ctx, cancel := context.WithTimeout(ctx, gitRemoteTimeout)
	defer cancel()
	rootOut, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return gitTargetEvidence{}, err
	}
	remoteOut, err := exec.CommandContext(ctx, "git", "-C", cwd, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return gitTargetEvidence{}, err
	}
	return gitTargetEvidence{RemoteURL: strings.TrimSpace(string(remoteOut)), Root: strings.TrimSpace(string(rootOut))}, nil
}
