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
	owner, repo, ok := parseOwnerRepo(url)
	if !ok {
		return "", false
	}
	return strings.ToLower(owner + "-" + repo), true
}

// parseOwnerRepo extracts the trailing owner/repo pair from a git remote URL.
// Handles the shapes git actually emits — https://host/owner/repo(.git),
// git@host:owner/repo.git, ssh://git@host[:port]/owner/repo.git — by reducing
// to the path portion and taking its last two non-empty segments. Returns
// ok=false for anything without at least an owner and a repo. Pure so the
// parsing edge cases are unit-tested directly.
func parseOwnerRepo(remote string) (owner, repo string, ok bool) {
	s := strings.TrimSpace(remote)
	s = strings.TrimSuffix(s, ".git")
	if s == "" {
		return "", "", false
	}

	// Reduce to the path portion (owner/repo[/...]).
	var path string
	switch {
	case strings.Contains(s, "://"):
		// scheme://[user@]host[:port]/owner/repo
		rest := s[strings.Index(s, "://")+3:]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			path = rest[slash+1:]
		}
	case strings.Contains(s, ":"):
		// scp-like: [user@]host:owner/repo
		path = s[strings.LastIndex(s, ":")+1:]
	default:
		path = s
	}

	segs := make([]string, 0, 4)
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	if len(segs) < 2 {
		return "", "", false
	}
	return segs[len(segs)-2], segs[len(segs)-1], true
}
