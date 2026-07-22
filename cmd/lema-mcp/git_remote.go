package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// git_remote.go — derive the target workspace from the repo itself (decision
// d_d9caf0 / c002bdc0). An org credential serves every repo in the org, so the
// workspace need not be per-repo CONFIG: the git remote's owner/repo forms the
// repo-anchored workspace slug (owner-repo), which the sync verifies against
// the credential's own /workspaces listing before writing anywhere. This turns
// the collector zero-config for multi-repo — an explicit LEMA_WORKSPACE_ID
// stays the override pin, not the requirement.
//
// The earlier rejection of git-remote parsing in the hook (d_dc7b06: exec cost
// under the 5s sync budget) is answered here by (a) deriving only when the env
// pin is unset — the configured case pays nothing — and (b) a fast local
// `git config` read (no network) bounded by gitRemoteTimeout, dwarfed by the
// listing round-trip the sync already makes.

const gitRemoteTimeout = 2 * time.Second

// gitRemoteURL reads the origin remote URL for cwd. A package var so tests can
// stub the exec without standing up a real git repo. Returns ok=false for a
// non-repo, a repo with no origin, or any git failure — the caller then skips
// derivation (fail-open).
var gitRemoteURL = func(cwd string) (string, bool) {
	if strings.TrimSpace(cwd) == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitRemoteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", false
	}
	return url, true
}

// deriveWorkspaceSlug turns the git remote in cwd into the repo-anchored
// workspace slug (owner-repo, lowercased) — the same slug the hosted workspace
// for a connected repo carries. Returns ok=false when cwd has no usable git
// remote; the sync then falls through to skip (never a guess).
func deriveWorkspaceSlug(cwd string) (string, bool) {
	url, ok := gitRemoteURL(cwd)
	if !ok {
		return "", false
	}
	identity, ok := repositoryIdentityFromRemote(url)
	if !ok {
		return "", false
	}
	return identity.Owner + "-" + identity.Name, true
}

// parseOwnerRepo extracts the trailing owner/repo pair from a git remote URL.
// Handles the shapes git actually emits — https://host/owner/repo(.git),
// git@host:owner/repo.git, ssh://git@host[:port]/owner/repo.git — by reducing
// to the path portion and taking its last two non-empty segments. Returns
// ok=false for anything without at least an owner and a repo. Pure so the
// parsing edge cases are unit-tested directly.
func parseOwnerRepo(remote string) (owner, repo string, ok bool) {
	identity, ok := repositoryIdentityFromRemote(remote)
	if !ok {
		return "", "", false
	}
	return identity.Owner, identity.Name, true
}

// gitCurrentBranch reads the checked-out branch for cwd. A package var so tests
// can stub the exec without a real repo. Returns ok=false for a non-repo, a
// detached HEAD (`git branch --show-current` prints nothing), or any git
// failure — the caller then sends an empty branch (never a guess), and rung 3
// (repo+branch) simply does not match while rung 4 / rung 7 still can.
var gitCurrentBranch = func(cwd string) (string, bool) {
	if strings.TrimSpace(cwd) == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitRemoteTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "branch", "--show-current")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" {
		return "", false // detached HEAD
	}
	return branch, true
}

// deriveRunGitContext derives the run's repo ("owner/name", lowercased to match
// the server's work_units.repo lowercase-at-write) and branch from cwd's git
// context — the association-ladder inputs for rung 3 (repo+branch) and rung 4
// (repo+worktree). Both are best-effort: any git failure yields an empty value
// and the run lands rung-7 exactly as before (fail-open). Reuses the same
// gitRemoteURL/parseOwnerRepo the workspace derivation uses (decision 5025ffb7,
// implementing d_d9caf0).
func deriveRunGitContext(cwd string) (repo, branch string) {
	if url, ok := gitRemoteURL(cwd); ok {
		if owner, name, ok := parseOwnerRepo(url); ok {
			repo = strings.ToLower(owner + "/" + name)
		}
	}
	if b, ok := gitCurrentBranch(cwd); ok {
		branch = b
	}
	return repo, branch
}
