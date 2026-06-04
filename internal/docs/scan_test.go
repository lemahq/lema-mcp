package docs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanDefaultRootsAndFiles(t *testing.T) {
	// The default scope is the contract the tab's first-run experience stands
	// on: a repo with docs/ + openspec/ + a README shows content with zero config.
	root := t.TempDir()
	writeFile(t, root, "docs/vision.md", "# Vision\n")
	writeFile(t, root, "docs/adr/0001-x.md", "# ADR-0001\n")
	writeFile(t, root, "openspec/specs/auth/spec.md", "# Auth\n")
	writeFile(t, root, "README.md", "# Readme\n")
	writeFile(t, root, "CLAUDE.md", "rules\n")
	writeFile(t, root, "src/code.md", "# not in scope\n")
	writeFile(t, root, "docs/img.png", "binary")

	got := scanFiles(root, Config{})
	want := []string{"CLAUDE.md", "README.md", "docs/adr/0001-x.md", "docs/vision.md", "openspec/specs/auth/spec.md"}
	if !slices.Equal(got, want) {
		t.Fatalf("scan = %v, want %v", got, want)
	}
}

func TestScanSkipsVendorTrees(t *testing.T) {
	// Vendored markdown is noise that taxes every query's retrieval quality —
	// the reason ADR-0055 rejected "index all repo markdown".
	root := t.TempDir()
	writeFile(t, root, "docs/real.md", "# Real\n")
	writeFile(t, root, "docs/node_modules/pkg/README.md", "# vendored\n")
	writeFile(t, root, "docs/.git/notes.md", "# git internals\n")

	got := scanFiles(root, Config{})
	if !slices.Equal(got, []string{"docs/real.md"}) {
		t.Fatalf("scan = %v, want [docs/real.md]", got)
	}
}

func TestScanSizeCapSkips(t *testing.T) {
	// A >1MB markdown file is a generated artifact, not docs; indexing it
	// would bloat memory and drown ranking. It must be skipped — and loudly
	// (the log assert lives in the store; here we pin the exclusion).
	root := t.TempDir()
	writeFile(t, root, "docs/big.md", "# big\n"+strings.Repeat("x", maxDocBytes))
	writeFile(t, root, "docs/ok.md", "# ok\n")

	got := scanFiles(root, Config{})
	if !slices.Equal(got, []string{"docs/ok.md"}) {
		t.Fatalf("scan = %v, want [docs/ok.md]", got)
	}
}

func TestScanConfigIncludeExclude(t *testing.T) {
	// Include = extra roots (repos keeping docs elsewhere opt in); exclude =
	// path prefixes (carve a private subtree out of a default root).
	root := t.TempDir()
	writeFile(t, root, "docs/pub.md", "# pub\n")
	writeFile(t, root, "docs/private/secret.md", "# secret\n")
	writeFile(t, root, "notes/idea.md", "# idea\n")

	var cfg Config
	cfg.Docs.Include = []string{"notes"}
	cfg.Docs.Exclude = []string{"docs/private"}
	got := scanFiles(root, cfg)
	want := []string{"docs/pub.md", "notes/idea.md"}
	if !slices.Equal(got, want) {
		t.Fatalf("scan = %v, want %v", got, want)
	}
}

func TestLoadConfigMissingAndMalformed(t *testing.T) {
	// Config is optional and malformed config must never take the engine down
	// (fail loud in the log, fall back to defaults) — the engine is the
	// workbench's sidecar; a typo in a config file cannot brick the app.
	root := t.TempDir()
	if c := loadConfig(root); len(c.Docs.Include) != 0 {
		t.Fatalf("missing config should be zero value, got %+v", c)
	}
	writeFile(t, root, ".lema/config.json", "{not json")
	if c := loadConfig(root); len(c.Docs.Include) != 0 || len(c.Docs.Exclude) != 0 {
		t.Fatalf("malformed config should fall back to zero value, got %+v", c)
	}
}
