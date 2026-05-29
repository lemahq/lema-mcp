package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	gogithub "github.com/google/go-github/v66/github"

	"github.com/lemahq/lema-mcp/internal/adr"
)

// githubRepoRe matches a GitHub repo reference: an optional scheme, the
// github.com host, then owner/repo (with an optional .git suffix or trailing
// slash). A bare "owner/repo" or a local subpath like "docs/adr" is NOT matched
// — remote fetch deliberately requires the github.com host so --repo stays
// unambiguous with the local --adr-dir filesystem path.
var githubRepoRe = regexp.MustCompile(`^(?:https?://)?github\.com/([^/\s]+)/([^/\s]+?)(?:\.git)?/?$`)

// adrDirCandidates are the common locations teams keep ADRs. When --adr-dir is
// not given, the local wedge probes these (filesystem) and the remote fetch
// probes them (GitHub API), using the first that contains matching files — so
// "point it at a repo" works across conventions without configuration.
var adrDirCandidates = []string{
	"docs/adr", "doc/adr", "docs/adrs", "docs/decisions",
	"docs/architecture/decisions", "architecture/decisions", "adr", ".adr",
}

// parseGitHubRepo extracts owner/repo from a github.com reference. ok=false for
// anything that isn't a github.com repo (a bare label, a local path), which
// routes main() to local --adr-dir mode.
func parseGitHubRepo(s string) (owner, repo string, ok bool) {
	m := githubRepoRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// discoverLocalADRDir returns the first common ADR directory that exists under
// root, or "" if none — so `lema-mcp` run in a repo root finds docs/adr, doc/adr,
// docs/decisions, etc. without requiring --adr-dir.
func discoverLocalADRDir(root string) string {
	for _, c := range adrDirCandidates {
		p := filepath.Join(root, c)
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			return p
		}
	}
	return ""
}

// fetchRemoteADRs pulls a repo's ADRs from GitHub and parses each file whose
// basename matches `match` — the network counterpart to adr.ParseDirMatching, so
// the wedge works on any public repo with no local checkout. If subdir is given
// it is used verbatim; otherwise the common ADR directories are probed. Public
// repos need no auth; GITHUB_TOKEN (if set) is used for a higher rate limit and
// private repos. Returns the parsed ADRs and the canonical github.com/owner/repo
// label.
func fetchRemoteADRs(ctx context.Context, owner, repo, subdir, ref string, match *regexp.Regexp) ([]adr.ADR, string, error) {
	client := gogithub.NewClient(nil)
	if tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); tok != "" {
		client = client.WithAuthToken(tok)
	}
	label := fmt.Sprintf("github.com/%s/%s", owner, repo)

	branch := strings.TrimSpace(ref)
	if branch == "" {
		info, _, err := client.Repositories.Get(ctx, owner, repo)
		if err != nil {
			return nil, "", fmt.Errorf("get %s (private repos need GITHUB_TOKEN): %w", label, err)
		}
		branch = info.GetDefaultBranch()
	}

	candidates := adrDirCandidates
	if s := strings.TrimSpace(subdir); s != "" {
		candidates = []string{s} // explicit --adr-dir: use it, don't guess
	}
	for _, sub := range candidates {
		adrs, err := fetchADRDir(ctx, client, owner, repo, sub, branch, match)
		if err != nil {
			return nil, "", err
		}
		if len(adrs) > 0 {
			return adrs, label, nil
		}
	}
	return nil, "", fmt.Errorf("no ADR files found in %s on %s (looked in: %s) — pass --adr-dir", label, branch, strings.Join(candidates, ", "))
}

// fetchADRDir lists one subdir and parses the matching files. A missing dir
// returns (nil, nil) so the caller can try the next candidate.
func fetchADRDir(ctx context.Context, client *gogithub.Client, owner, repo, subdir, branch string, match *regexp.Regexp) ([]adr.ADR, error) {
	_, entries, resp, err := client.Repositories.GetContents(ctx, owner, repo, subdir, &gogithub.RepositoryContentGetOptions{Ref: branch})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return nil, nil // not this dir — let the caller try the next candidate
		}
		return nil, fmt.Errorf("list %s/%s/%s: %w", owner, repo, subdir, err)
	}
	const maxFiles = 500
	var out []adr.ADR
	for _, e := range entries {
		if e.GetType() != "file" || !match.MatchString(e.GetName()) {
			continue
		}
		if len(out) >= maxFiles {
			fmt.Fprintf(os.Stderr, "lema-mcp: capped at %d ADR files in %s/%s\n", maxFiles, subdir, "(narrow --adr-dir)")
			break
		}
		fc, _, _, err := client.Repositories.GetContents(ctx, owner, repo, e.GetPath(), &gogithub.RepositoryContentGetOptions{Ref: branch})
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", e.GetPath(), err)
		}
		body, err := fc.GetContent()
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", e.GetPath(), err)
		}
		parsed, err := adr.ParseBytes([]byte(body), e.GetName(), fmt.Sprintf("github.com/%s/%s/%s", owner, repo, e.GetPath()))
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.GetPath(), err)
		}
		out = append(out, parsed)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}
