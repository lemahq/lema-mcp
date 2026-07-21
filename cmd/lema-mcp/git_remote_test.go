package main

import "testing"

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
