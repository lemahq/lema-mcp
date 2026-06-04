package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/adr"
	"github.com/lemahq/lema-mcp/internal/docs"
	"github.com/lemahq/lema-mcp/internal/source"
)

// setupDocsWorld points the package globals at a temp repo with one ADR and
// one plain doc, both mentioning "wombat", so scope behavior is observable.
func setupDocsWorld(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "guide.md"),
		[]byte("# Guide\n\n## Wombat handling\n\nwombat docs text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldSrc := src
	src = source.NewLocal([]adr.ADR{{
		Number: 1, Title: "Wombat policy", Status: "accepted",
		Body: "## Decision\n\nwe chose the wombat approach\n",
	}})
	// A real (empty) capture store, not nil: httpSearch's localSearchROI path
	// is not guaranteed nil-safe, and serve mode always has one in production.
	oldCapture := capture
	cs, err := source.NewCaptureStore(filepath.Join(root, ".lema", "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	capture = cs
	ds := docs.NewStore(root, "docs/adr")
	if _, err := ds.Scan(); err != nil {
		t.Fatal(err)
	}
	oldDocs := docsStore
	docsStore = ds
	t.Cleanup(func() { src = oldSrc; capture = oldCapture; docsStore = oldDocs })
}

func searchBody(t *testing.T, url string) searchOutput {
	t.Helper()
	rec := httptest.NewRecorder()
	httpSearch(rec, httptest.NewRequest("GET", url, nil))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out searchOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestHTTPSearchDefaultScopeIsDecisionsOnly(t *testing.T) {
	// THE regression pin of ADR-0055: with no scope param, /api/search returns
	// exactly what it returned before the docs feature — claims, no docs —
	// so search_decisions, check_decided, and the EnforcementRail are
	// provably unaffected.
	setupDocsWorld(t)
	out := searchBody(t, "/api/search?q=wombat")
	if len(out.Claims) == 0 {
		t.Fatal("default scope lost its decision claims")
	}
	if len(out.Docs) != 0 {
		t.Fatalf("default scope must not include docs hits, got %+v", out.Docs)
	}
}

func TestHTTPSearchScopeAllReturnsBothLabeled(t *testing.T) {
	setupDocsWorld(t)
	out := searchBody(t, "/api/search?q=wombat&scope=all")
	if len(out.Claims) == 0 || len(out.Docs) == 0 {
		t.Fatalf("scope=all must return both: claims=%d docs=%d", len(out.Claims), len(out.Docs))
	}
	if out.Docs[0].Path != "docs/guide.md" {
		t.Fatalf("docs hit = %+v", out.Docs[0])
	}
}

func TestHTTPSearchScopeDocsOnly(t *testing.T) {
	setupDocsWorld(t)
	out := searchBody(t, "/api/search?q=wombat&scope=docs")
	if len(out.Claims) != 0 || len(out.Docs) == 0 {
		t.Fatalf("scope=docs must return docs only: claims=%d docs=%d", len(out.Claims), len(out.Docs))
	}
}

func TestHTTPDocsAndDoc(t *testing.T) {
	setupDocsWorld(t)
	rec := httptest.NewRecorder()
	httpDocs(rec, httptest.NewRequest("GET", "/api/docs", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "docs/guide.md") {
		t.Fatalf("docs listing: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	httpDoc(rec, httptest.NewRequest("GET", "/api/doc?path=docs%2Fguide.md", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "wombat docs text") {
		t.Fatalf("doc body: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	httpDoc(rec, httptest.NewRequest("GET", "/api/doc?path=..%2F..%2Fetc%2Fpasswd", nil))
	if rec.Code != 404 {
		t.Fatalf("traversal path must 404, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	httpDoc(rec, httptest.NewRequest("GET", "/api/doc?path=docs%2Fguide.md&section=wombat+handling", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "wombat docs text") {
		t.Fatalf("section: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPDocsNilStoreIsEmptyNotError(t *testing.T) {
	// Hosted/remote runs have no docs store; the GUI must get an empty
	// listing, not a 500 — same "serve empty" stance as the no-decisions case.
	old := docsStore
	docsStore = nil
	t.Cleanup(func() { docsStore = old })
	rec := httptest.NewRecorder()
	httpDocs(rec, httptest.NewRequest("GET", "/api/docs", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "\"docs\":[]") {
		t.Fatalf("nil store: %d %s", rec.Code, rec.Body.String())
	}
}
