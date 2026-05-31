package openspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "openspec")
	write(t, filepath.Join(root, "specs", "auth", "spec.md"),
		"# Auth\n\n## Requirements\n\n- Sessions expire after 24h.\n")
	write(t, filepath.Join(root, "changes", "add-login", "proposal.md"),
		"## Why\n\nUsers need to sign in.\n\n## What Changes\n\nAdd a /login route backed by sessions.\n")
	write(t, filepath.Join(root, "changes", "add-login", "design.md"),
		"# Design\n\n## Alternatives\n\n- JWT in localStorage was rejected because XSS can exfiltrate it; httpOnly session cookies were chosen.\n")
	write(t, filepath.Join(root, "changes", "archive", "old-thing", "proposal.md"),
		"## Why\n\nLegacy.\n")

	recs, err := ParseDir(root, 100)
	if err != nil {
		t.Fatalf("ParseDir: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("want 3 records, got %d", len(recs))
	}

	by := map[string]int{}
	for i, r := range recs {
		by[r.Ref] = i
		if r.Number != 100+i {
			t.Errorf("%s number = %d, want %d", r.Ref, r.Number, 100+i)
		}
	}
	for _, want := range []string{"openspec/spec/auth", "openspec/change/add-login", "openspec/change/old-thing"} {
		if _, ok := by[want]; !ok {
			t.Errorf("missing ref %q (got refs %v)", want, by)
		}
	}

	if recs[by["openspec/change/old-thing"]].Status != "accepted" {
		t.Errorf("archived change should be accepted")
	}
	if recs[by["openspec/change/add-login"]].Status != "proposed" {
		t.Errorf("active change should be proposed")
	}
	if recs[by["openspec/spec/auth"]].Status != "accepted" {
		t.Errorf("spec should be accepted")
	}

	login := recs[by["openspec/change/add-login"]]
	if !strings.Contains(login.Body, "## Design") || !strings.Contains(login.Body, "rejected because XSS") {
		t.Errorf("change body did not fold in the design:\n%s", login.Body)
	}
	if login.Title != "Change: Add login" {
		t.Errorf("title = %q, want %q", login.Title, "Change: Add login")
	}
}

// TestServesThroughSource is the integration check: an OpenSpec change served
// through source.Local surfaces its design alternative as a 'rejected' atom
// carrying the openspec/ ref — proving the Ref fallback + sectionType wiring.
func TestServesThroughSource(t *testing.T) {
	root := filepath.Join(t.TempDir(), "openspec")
	write(t, filepath.Join(root, "changes", "add-login", "proposal.md"),
		"## Why\n\nUsers need to sign in.\n\n## What Changes\n\nAdd a /login route.\n")
	write(t, filepath.Join(root, "changes", "add-login", "design.md"),
		"## Alternatives\n\n- JWT in localStorage rejected because XSS can exfiltrate the token; httpOnly session cookies chosen.\n")

	recs, err := ParseDir(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	src := source.NewLocal(recs)
	atoms, err := src.Search(t.Context(), "jwt localstorage rejected", 8)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range atoms {
		if a.Type == "rejected" && a.Ref == "openspec/change/add-login" &&
			strings.Contains(strings.ToLower(a.Text), "jwt") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a rejected atom with ref openspec/change/add-login; got %+v", atoms)
	}
}
