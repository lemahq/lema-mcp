package docs

import "testing"

// Intent: scope.go is the ONE definition of project-doc scope, shared by the
// local walker and the server-side intake scanner (decision a827fc9d). These
// tests pin the POLICY itself — if InScope drifts, both consumers drift
// together and silently, so the policy needs its own pins rather than only
// being exercised through scanFiles.

func TestInScopeDefaultRoots(t *testing.T) {
	var cfg Config
	in := []string{
		"docs/vision.md",
		"docs/adr/0142-feature-specs.md",
		"openspec/changes/x.md",
		"README.md",
		"CLAUDE.md",
		"AGENTS.md",
	}
	for _, p := range in {
		if !InScope(p, cfg) {
			t.Errorf("expected %q in scope", p)
		}
	}

	// The whole point of ADR-0055 / d_8564d1: scope is known roots, NOT all
	// repo markdown. A file outside them is out even though it is markdown.
	out := []string{
		"CHANGELOG.md",
		"src/notes.md",
		"packages/cli/README.md",
		"docs/logo.png",
		"",
	}
	for _, p := range out {
		if InScope(p, cfg) {
			t.Errorf("expected %q OUT of scope — scope is known roots, never all repo markdown", p)
		}
	}
}

func TestInScopeConfigIncludeIsTheOptIn(t *testing.T) {
	var cfg Config
	if InScope("handbook/process.md", cfg) {
		t.Fatal("precondition: handbook/ is not a default root")
	}
	// .lema/config.json include is the escape hatch for an unusual repo —
	// the second one alongside the roadmap (product-spec, Unit 1).
	cfg.Docs.Include = []string{"handbook"}
	if !InScope("handbook/process.md", cfg) {
		t.Error("a config include must bring an unusual root into scope")
	}
	// A single-file include, not just a directory.
	cfg = Config{}
	cfg.Docs.Include = []string{"CONTRIBUTING.md"}
	if !InScope("CONTRIBUTING.md", cfg) {
		t.Error("a config include naming one file must bring that file into scope")
	}
	if InScope("CONTRIBUTING-OLD.md", cfg) {
		t.Error("an include must match a path or a directory prefix, never a string prefix")
	}
}

func TestInScopeExcludeCarvesOutOfADefaultRoot(t *testing.T) {
	var cfg Config
	cfg.Docs.Exclude = []string{"docs/vendor"}
	if InScope("docs/vendor/thing.md", cfg) {
		t.Error("an exclude must carve a subtree out of a default root")
	}
	if !InScope("docs/vision.md", cfg) {
		t.Error("an exclude must not remove its siblings")
	}
	// Prefix semantics are on path SEGMENTS, not raw strings — otherwise
	// excluding "docs/vendor" would also drop "docs/vendoring-policy.md".
	if !InScope("docs/vendoring-policy.md", cfg) {
		t.Error("exclude must match path segments, not string prefixes")
	}
}

func TestInScopeNeverDescendsSkipDirs(t *testing.T) {
	var cfg Config
	cfg.Docs.Include = []string{"."} // even a maximally broad opt-in
	for _, p := range []string{
		"docs/node_modules/pkg/README.md",
		"vendor/lib/docs/guide.md",
		"docs/dist/out.md",
		"docs/.git/notes.md",
	} {
		if InScope(p, cfg) {
			t.Errorf("expected %q OUT of scope — vendored/generated markdown is the noise ADR-0055 rejected", p)
		}
	}
}

func TestOutOfTreeRejectsEscapes(t *testing.T) {
	for _, p := range []string{"../secrets", "/etc", "docs/../../up"} {
		if !OutOfTree(p) {
			t.Errorf("expected %q flagged out-of-tree", p)
		}
	}
	for _, p := range []string{"docs", "handbook/process.md", "./docs"} {
		if OutOfTree(p) {
			t.Errorf("expected %q accepted as in-tree", p)
		}
	}
}

func TestParseConfigMalformedIsAnError(t *testing.T) {
	if _, err := ParseConfig([]byte("{not json")); err == nil {
		t.Error("a malformed config must surface as an error so the caller can DISCLOSE it")
	}
	c, err := ParseConfig(nil)
	if err != nil || len(c.Docs.Include) != 0 {
		t.Errorf("empty config = zero value, no error; got %v %v", c, err)
	}
}
