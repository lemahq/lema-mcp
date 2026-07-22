package main

import (
	"strings"
	"testing"
)

func TestParseOwnerRepo(t *testing.T) {
	cases := []struct {
		in          string
		owner, repo string
		ok          bool
	}{
		// The shapes git actually emits.
		{"https://github.com/lemahq/lema.git", "lemahq", "lema", true},
		{"https://github.com/lemahq/lema", "lemahq", "lema", true},
		{"git@github.com:lemahq/lema.git", "lemahq", "lema", true},
		{"git@github.com:lemahq/lema", "lemahq", "lema", true},
		{"ssh://git@github.com/lemahq/lema.git", "lemahq", "lema", true},
		{"ssh://git@github.com:22/lemahq/lema.git", "lemahq", "lema", true},
		{"https://github.com/lemahq/lema-mcp.git", "lemahq", "lema-mcp", true},
		// A hyphenated repo name survives (the slug is owner-repo, so lema-mcp
		// → lemahq-lema-mcp downstream).
		{"git@github.com:lemahq/lema-mcp.git", "lemahq", "lema-mcp", true},
		// Trailing slash / gitlab-style subgroup: take the last two segments.
		{"https://github.com/lemahq/lema/", "lemahq", "lema", true},
		{"git@gitlab.com:group/sub/repo.git", "sub", "repo", true},
		// Not derivable.
		{"", "", "", false},
		{"not-a-url", "", "", false},
		{"https://github.com/onlyowner", "", "", false},
		{"https://github.com/onlyowner.git", "", "", false},
	}
	for _, c := range cases {
		owner, repo, ok := parseOwnerRepo(c.in)
		if ok != c.ok || owner != c.owner || repo != c.repo {
			t.Errorf("parseOwnerRepo(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, owner, repo, ok, c.owner, c.repo, c.ok)
		}
	}
}

func TestDeriveWorkspaceSlug(t *testing.T) {
	// Case is folded so a mixed-case remote still matches the lowercase slug the
	// workspace carries.
	restore := gitRemoteURL
	t.Cleanup(func() { gitRemoteURL = restore })

	gitRemoteURL = func(string) (string, bool) { return "https://github.com/LemaHQ/Lema.git", true }
	if slug, ok := deriveWorkspaceSlug("/anywhere"); !ok || slug != "lemahq-lema" {
		t.Fatalf("derive = (%q,%v), want (lemahq-lema,true)", slug, ok)
	}

	gitRemoteURL = func(string) (string, bool) { return "", false }
	if slug, ok := deriveWorkspaceSlug("/anywhere"); ok || slug != "" {
		t.Fatalf("no remote must not derive, got (%q,%v)", slug, ok)
	}
}

func TestGitRemoteNormalizationKeepsHostsDistinctAndRedactsSecrets(t *testing.T) {
	for _, tc := range []struct {
		remote    string
		canonical string
		slug      string
	}{
		{"https://token:secret@github.com:443/acme/api.git?token=leak#fragment", "git:github.com/acme/api", "acme-api"},
		{"ssh://git@github.acme.internal:2222/acme/api.git?token=leak#fragment", "git:github.acme.internal:2222/acme/api", "acme-api"},
		{"git@github.acme.internal:acme/api.git?token=leak#fragment", "git:github.acme.internal/acme/api", "acme-api"},
	} {
		identity, ok := repositoryIdentityFromRemote(tc.remote)
		if !ok || identity.Canonical != tc.canonical {
			t.Fatalf("repositoryIdentityFromRemote(%q) = (%#v, %v), want %q", tc.remote, identity, ok, tc.canonical)
		}
		if strings.Contains(identity.Canonical, "secret") || strings.Contains(identity.Canonical, "leak") {
			t.Fatalf("canonical identity leaked remote secret: %q", identity.Canonical)
		}
		owner, repo, ok := parseOwnerRepo(tc.remote)
		if !ok || owner+"-"+repo != tc.slug {
			t.Fatalf("parseOwnerRepo(%q) = (%q, %q, %v), want compatibility slug %q", tc.remote, owner, repo, ok, tc.slug)
		}
	}
}

func TestDeriveRunGitContext(t *testing.T) {
	restoreRemote, restoreBranch := gitRemoteURL, gitCurrentBranch
	t.Cleanup(func() { gitRemoteURL, gitCurrentBranch = restoreRemote, restoreBranch })

	// Full context: repo is lowercased owner/name (matches work_units.repo's
	// lowercase-at-write), branch verbatim.
	gitRemoteURL = func(string) (string, bool) { return "git@github.com:LemaHQ/Lema.git", true }
	gitCurrentBranch = func(string) (string, bool) { return "feat/x", true }
	if repo, branch := deriveRunGitContext("/anywhere"); repo != "lemahq/lema" || branch != "feat/x" {
		t.Fatalf("derive = (%q,%q), want (lemahq/lema, feat/x)", repo, branch)
	}

	// Each field is independent + fail-open: no remote → empty repo but branch
	// still resolves; detached HEAD → empty branch but repo still resolves.
	gitRemoteURL = func(string) (string, bool) { return "", false }
	gitCurrentBranch = func(string) (string, bool) { return "main", true }
	if repo, branch := deriveRunGitContext("/anywhere"); repo != "" || branch != "main" {
		t.Fatalf("no remote = (%q,%q), want (\"\", main)", repo, branch)
	}
	gitRemoteURL = func(string) (string, bool) { return "https://github.com/o/r.git", true }
	gitCurrentBranch = func(string) (string, bool) { return "", false }
	if repo, branch := deriveRunGitContext("/anywhere"); repo != "o/r" || branch != "" {
		t.Fatalf("detached = (%q,%q), want (o/r, \"\")", repo, branch)
	}
}
