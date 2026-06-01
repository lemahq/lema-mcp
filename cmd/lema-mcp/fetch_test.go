package main

import "testing"

// parseGitHubRepo must accept the documented github.com forms (the landing-page
// command uses github.com/org/repo) and reject anything that should stay local
// — a bare owner/repo, a local subpath, a label — so --repo never mistakes a
// directory like docs/adr for a remote fetch.
func TestParseGitHubRepo(t *testing.T) {
	remote := map[string][2]string{
		"github.com/lemahq/lema":         {"lemahq", "lema"},
		"https://github.com/lemahq/lema": {"lemahq", "lema"},
		"http://github.com/org/repo.git": {"org", "repo"},
		"github.com/org/repo/":           {"org", "repo"},
	}
	for in, want := range remote {
		o, r, ok := parseGitHubRepo(in)
		if !ok || o != want[0] || r != want[1] {
			t.Errorf("parseGitHubRepo(%q) = (%q,%q,%v), want (%q,%q,true)", in, o, r, ok, want[0], want[1])
		}
	}
	local := []string{"", "docs/adr", "lemahq/lema", "./decisions", "some-label", "a/b/c"}
	for _, in := range local {
		if o, r, ok := parseGitHubRepo(in); ok {
			t.Errorf("parseGitHubRepo(%q) = (%q,%q,true); want local mode (ok=false)", in, o, r)
		}
	}
}
