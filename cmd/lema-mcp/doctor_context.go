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
	APIURL              string
	Token               string
	ExplicitWorkspaceID string
	CWD                 string
	ReadGit             func(context.Context, string) (gitTargetEvidence, error)
	Output              io.Writer
	Client              *http.Client
}

type doctorConfig struct {
	APIURL              string
	Token               string
	ExplicitWorkspaceID string
}

const doctorHostedLookupAction = "verify hosted API reachability and response compatibility"

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
		options.ReadGit = readContextGitEvidence
	}

	if strings.TrimSpace(options.APIURL) == "" || strings.TrimSpace(options.Token) == "" {
		writeDoctorFailure(out, resolutionUnresolved, "credentials", "configure LEMA_API_URL and LEMA_API_TOKEN")
		return fmt.Errorf("target context unresolved")
	}

	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	explicitWorkspaceID := strings.TrimSpace(options.ExplicitWorkspaceID)
	if explicitWorkspaceID != "" {
		var err error
		explicitWorkspaceID, err = resolveWorkspaceValueUUID(ctx, client, options.APIURL, options.Token, explicitWorkspaceID)
		if err != nil {
			writeDoctorFailure(out, targetResolutionStatusFromError(err), targetResolutionRungFromError(err), doctorActionForLookupError(err))
			return fmt.Errorf("target context %s", targetResolutionStatusFromError(err))
		}
	}
	var localAssociation *targetContext
	if explicitWorkspaceID == "" {
		association, found, err := loadContextAssociation(options.CWD, options.ReadGit)
		if err != nil {
			if found && isContextAssociationStale(err) {
				writeDoctorFailure(out, resolutionStale, "local_association", "run lema-mcp context unlink to preserve a backup, then link this repository")
				return fmt.Errorf("target context stale")
			}
			writeDoctorFailure(out, resolutionUnresolved, "local_association", doctorHostedLookupAction)
			return fmt.Errorf("target context unresolved")
		}
		if found {
			localAssociation = &association
		}
	}
	resolver := newHostedTargetResolver(client, options.APIURL, options.Token)
	resolver.readGit = options.ReadGit
	result, err := resolver.Resolve(ctx, resolveTargetInput{
		APIURL:                options.APIURL,
		CredentialFingerprint: credentialFingerprint(options.Token),
		ExplicitWorkspaceID:   explicitWorkspaceID,
		CWD:                   options.CWD,
		LocalAssociation:      localAssociation,
		PersistedAssociation:  localAssociation != nil,
	})
	if err != nil {
		status := targetResolutionStatusFromError(err)
		writeDoctorFailure(out, status, targetResolutionRungFromError(err), doctorActionForLookupError(err))
		return fmt.Errorf("target context %s", status)
	}
	writeDoctorContext(out, result)
	if result.Status != resolutionResolved {
		return fmt.Errorf("target context %s", result.Status)
	}
	return nil
}

func doctorActionForLookupError(err error) string {
	if targetResolutionStatusFromError(err) == resolutionUnresolved {
		return doctorHostedLookupAction
	}
	return ""
}

func runDoctorContextCommand(args []string) error {
	if len(args) != 1 || args[0] != "context" {
		return fmt.Errorf("usage: lema-mcp doctor context")
	}
	config, err := resolveDoctorConfig()
	if err != nil {
		// Do not propagate a filesystem error into main's stderr: it can include
		// a private home directory. The command's own trace gives one safe action.
		return runDoctorContext(context.Background(), doctorContextOptions{})
	}
	return runDoctorContext(context.Background(), doctorContextOptions{APIURL: config.APIURL, Token: config.Token, ExplicitWorkspaceID: config.ExplicitWorkspaceID})
}

func resolveDoctorConfig() (doctorConfig, error) {
	config := doctorConfig{
		APIURL:              strings.TrimSpace(os.Getenv("LEMA_API_URL")),
		Token:               strings.TrimSpace(os.Getenv("LEMA_API_TOKEN")),
		ExplicitWorkspaceID: strings.TrimSpace(os.Getenv(workspaceIDEnv)),
	}
	needsConnectionFallback := config.APIURL == "" || config.Token == ""
	creds, err := readCredentialsFile(credentialsPath())
	if err != nil {
		if needsConnectionFallback {
			return config, err
		}
		return config, nil
	}
	if creds == nil {
		return config, nil
	}
	if config.APIURL == "" {
		config.APIURL = strings.TrimSpace(creds["LEMA_API_URL"])
	}
	if config.Token == "" {
		config.Token = strings.TrimSpace(creds["LEMA_API_TOKEN"])
	}
	if config.ExplicitWorkspaceID == "" {
		config.ExplicitWorkspaceID = strings.TrimSpace(creds[workspaceIDEnv])
	}
	return config, nil
}

func writeDoctorContext(out io.Writer, result resolutionResult) {
	fmt.Fprintln(out, "identity       unavailable")
	if result.Status == resolutionResolved {
		fmt.Fprintln(out, "rung           "+result.Context.ResolvedBy)
	} else {
		fmt.Fprintln(out, "rung           target_resolver")
	}
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
		case "local_root_hash":
			fmt.Fprintln(out, "local root hash  "+evidence.Value)
		}
	}
	fmt.Fprintln(out, "result         resolved by "+context.ResolvedBy)
}

func writeDoctorFailure(out io.Writer, status resolutionStatus, rung, action string) {
	fmt.Fprintln(out, "identity       unavailable")
	fmt.Fprintln(out, "rung           "+rung)
	fmt.Fprintln(out, "result         "+string(status))
	if action == "" {
		action = doctorCorrectiveAction(status)
	}
	fmt.Fprintln(out, "action         "+action)
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
		return "run lema-mcp context link --project <project-id> --repository <repository-id>"
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
