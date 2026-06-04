package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, root string) *Store {
	t.Helper()
	s := NewStore(root, "docs/adr")
	if _, err := s.Scan(); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreListGroups(t *testing.T) {
	// Group derivation is what lets the UI show ADRs/openspec once (rich, via
	// /api/decisions) without double-listing them as plain docs.
	root := t.TempDir()
	writeFile(t, root, "docs/adr/0001-x.md", "# ADR-0001: X\n\n## Decision\n\nwe chose x\n")
	writeFile(t, root, "openspec/specs/auth/spec.md", "# Auth\n\nspec text\n")
	writeFile(t, root, "docs/superpowers/specs/2026-01-01-y-design.md", "# Y design\n\ndesign text\n")
	writeFile(t, root, "README.md", "# Readme\n\nhello\n")
	s := newTestStore(t, root)

	groups := map[string]string{}
	for _, d := range s.List() {
		groups[d.Path] = d.Group
	}
	want := map[string]string{
		"docs/adr/0001-x.md":                            "adr",
		"openspec/specs/auth/spec.md":                   "openspec",
		"docs/superpowers/specs/2026-01-01-y-design.md": "spec",
		"README.md":                                     "doc",
	}
	for p, g := range want {
		if groups[p] != g {
			t.Fatalf("group(%s) = %q, want %q (all: %v)", p, groups[p], g, groups)
		}
	}
}

func TestStoreSearchFindsHeadingPhrase(t *testing.T) {
	// The chunk index searches heading trails WITH the body — the full-text
	// recall the atom search lacks (ADR-0053 documented the heading-phrase
	// gap; ADR-0055 closes the search-side half of it).
	root := t.TempDir()
	writeFile(t, root, "docs/guide.md", "# Guide\n\n## Capture nudge calibration\n\nbody about thresholds\n")
	s := newTestStore(t, root)

	hits := s.Search("capture nudge calibration", 8)
	if len(hits) == 0 {
		t.Fatal("heading-phrase query returned nothing — trail text must be searchable")
	}
	if hits[0].Path != "docs/guide.md" {
		t.Fatalf("top hit = %+v", hits[0])
	}
}

func TestStoreGetAndSection(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "docs/guide.md", "# Guide\n\nintro\n\n## Setup\n\nsetup body\n\n### Tokens\n\ntoken body\n\n## Usage\n\nusage body\n")
	s := newTestStore(t, root)

	d, body, ok := s.Get("docs/guide.md")
	if !ok || d.Title != "Guide" || !strings.Contains(body, "usage body") {
		t.Fatalf("Get = %+v ok=%v", d, ok)
	}
	// Section returns the named section AND its children (Tokens nests under
	// Setup) — an agent asking for "Setup" needs the whole subtree to act.
	sec, ok := s.Section("docs/guide.md", "setup")
	if !ok {
		t.Fatal("Section(setup) not found — lookup must be case-insensitive")
	}
	if !strings.Contains(sec, "setup body") || !strings.Contains(sec, "token body") {
		t.Fatalf("section must include children: %q", sec)
	}
	if strings.Contains(sec, "usage body") {
		t.Fatalf("section must not leak the sibling section: %q", sec)
	}
	if _, ok := s.Section("docs/guide.md", "nope"); ok {
		t.Fatal("unknown section must report not-found, not empty success")
	}
}

func TestStoreGetUnknownPathIsNotFound(t *testing.T) {
	// Get serves from the scanned in-memory set only — the lookup IS the
	// path-traversal guard. A request-supplied path never touches the fs.
	root := t.TempDir()
	writeFile(t, root, "docs/a.md", "# A\n")
	s := newTestStore(t, root)
	if _, _, ok := s.Get("../../etc/passwd"); ok {
		t.Fatal("non-indexed path must be not-found")
	}
}

func TestStoreSweepPicksUpEditsAndDeletes(t *testing.T) {
	// The lazy sweep is the freshness contract: an edited doc is at most
	// sweepEvery stale, a deleted doc disappears — without a file watcher.
	root := t.TempDir()
	writeFile(t, root, "docs/a.md", "# A\n\nold text\n")
	writeFile(t, root, "docs/b.md", "# B\n\nb text\n")
	s := newTestStore(t, root)

	now := time.Now()
	s.now = func() time.Time { return now }

	writeFile(t, root, "docs/a.md", "# A\n\nnew text entirely\n")
	// Backdate-proof: bump mtime explicitly so the change is visible even on
	// coarse-mtime filesystems.
	if err := os.Chtimes(filepath.Join(root, "docs", "a.md"), now.Add(time.Hour), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "docs", "b.md")); err != nil {
		t.Fatal(err)
	}

	// Within the debounce window nothing changes (no fs churn per request).
	// "entirely" exists ONLY in the new content — a query term present in the
	// old content would match stale chunks and fake a pass/fail either way.
	if hits := s.Search("entirely", 8); len(hits) != 0 {
		t.Fatal("sweep ran inside the debounce window")
	}
	// …after it, the edit and the delete are both visible.
	now = now.Add(sweepEvery + time.Second)
	if hits := s.Search("entirely", 8); len(hits) == 0 {
		t.Fatal("edited content not picked up by sweep")
	}
	if _, _, ok := s.Get("docs/b.md"); ok {
		t.Fatal("deleted doc still served after sweep")
	}
}
