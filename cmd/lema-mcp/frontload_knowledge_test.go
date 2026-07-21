package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
)

func kfWire(artifact, state, detail string) *kfAuditWire {
	return &kfAuditWire{Artifact: artifact, State: state, Detail: detail}
}

// The knowledge frontload block (decision e886b49f) is the agent seat: it
// exists so an agent stops trusting a dead rule BEFORE acting on it. The
// honesty invariants pinned here: zero bytes when nothing is stale (never an
// all-clear), at most 3 items each carrying a verbatim quote + one fact, and
// the fixed abstention closer — the block never claims to verify the rest.
func TestRenderKnowledgeFrontloadZeroBytesWhenClean(t *testing.T) {
	out := renderKnowledgeFrontload([]kfAuditFile{
		{Path: "CLAUDE.md", AsOf: "2026-07-14T09:00:00Z", Rules: []kfAuditRule{
			{Text: "- fine rule", Headline: "fresh-as-of"},
			{Text: "- quiet rule", Headline: "unanchored"},
			{Text: "- unjudged rule", Headline: "unverified"},
		}},
	})
	if out != "" {
		t.Fatalf("clean audit rendered %q, want zero bytes (silence is not an all-clear, but noise is worse)", out)
	}
}

func TestRenderKnowledgeFrontloadStaleItems(t *testing.T) {
	out := renderKnowledgeFrontload([]kfAuditFile{
		{Path: "CLAUDE.md", AsOf: "2026-07-14T09:00:00Z", Rules: []kfAuditRule{
			{Text: "- packages/renderer is our Astro renderer", Headline: "stale",
				Wire: kfWire("packages/renderer", "tripped", "path deleted")},
			{Text: "- use shadcn/ui per ADR-0015", Headline: "stale",
				Anchor: &kfAuditAnchor{Title: "shadcn/ui", Status: "superseded"}},
			{Text: "- fine rule", Headline: "fresh-as-of"},
		}},
	})
	if out == "" {
		t.Fatal("stale rules rendered nothing")
	}
	for _, want := range []string{
		"Knowledge-file audit from lema",
		"2 rule(s)",
		"verify before relying",
		`CLAUDE.md: "- packages/renderer is our Astro renderer"`,
		"packages/renderer", "path deleted",
		"shadcn/ui", "superseded",
		"This is not a verification of the files' other rules",
		"search_decisions / check_decided",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("block missing %q:\n%s", want, out)
		}
	}
}

func TestRenderKnowledgeFrontloadCapsAtThreeAndTruncatesQuotes(t *testing.T) {
	long := "- " + strings.Repeat("x", 300)
	var rules []kfAuditRule
	for range 5 {
		rules = append(rules, kfAuditRule{Text: long, Headline: "stale",
			Wire: kfWire("a", "tripped", "gone")})
	}
	out := renderKnowledgeFrontload([]kfAuditFile{{Path: "AGENTS.md", AsOf: "2026-07-14T09:00:00Z", Rules: rules}})
	if !strings.Contains(out, "5 rule(s)") {
		t.Errorf("count must state ALL stale rules even when items cap at 3:\n%s", out)
	}
	if n := strings.Count(out, "AGENTS.md:"); n != 3 {
		t.Errorf("items = %d, want capped at 3", n)
	}
	if strings.Contains(out, strings.Repeat("x", 200)) {
		t.Errorf("quote not truncated")
	}
}

func TestFrontloadRunnerInjectsKnowledgeEvenWhenAskAbstains(t *testing.T) {
	r := frontloadRunner{
		ask: func(context.Context, string) (source.AskResult, error) {
			return source.AskResult{}, nil // server abstains
		},
		knowledge: func(context.Context) ([]kfAuditFile, error) {
			return []kfAuditFile{{Path: "CLAUDE.md", AsOf: "2026-07-14T09:00:00Z", Rules: []kfAuditRule{
				{Text: "- dead rule", Headline: "stale", Wire: kfWire("gone/path", "tripped", "path deleted")},
			}}}, nil
		},
		canQuery: true,
	}
	out := r.run(context.Background(), frontloadInput{Prompt: "how do we render docs?"})
	if !strings.Contains(out, "Knowledge-file audit from lema") {
		t.Fatalf("knowledge block missing when ask abstained:\n%s", out)
	}
}

func TestFrontloadRunnerKnowledgeFailsOpen(t *testing.T) {
	r := frontloadRunner{
		ask: func(context.Context, string) (source.AskResult, error) {
			return source.AskResult{}, nil
		},
		knowledge: func(context.Context) ([]kfAuditFile, error) {
			return nil, errors.New("api down")
		},
		canQuery: true,
	}
	if out := r.run(context.Background(), frontloadInput{Prompt: "anything"}); out != "" {
		t.Fatalf("knowledge failure must inject nothing, got %q", out)
	}
}

func TestKnowledgeFetcherDarkCacheSkipsRepeat404(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := newKnowledgeFetcher(srv.Client(), srv.URL, "tok", "ws1")
	f.markerPath = filepath.Join(t.TempDir(), "dark-marker")

	if _, err := f.fetch(context.Background()); err == nil {
		t.Fatal("first fetch: want an error on 404")
	}
	if _, err := f.fetch(context.Background()); !errors.Is(err, errKnowledgeDark) {
		t.Fatalf("second fetch = %v, want the cached-dark skip", err)
	}
	if hits != 1 {
		t.Fatalf("server hits = %d, want 1 — a per-prompt hook must not re-pay the dark 404", hits)
	}

	// Past the TTL the marker expires and the fetch goes to the network again.
	old := time.Now().Add(-knowledgeDarkTTL - time.Minute)
	if err := os.Chtimes(f.markerPath, old, old); err != nil {
		t.Fatalf("age marker: %v", err)
	}
	_, _ = f.fetch(context.Background())
	if hits != 2 {
		t.Fatalf("server hits = %d, want 2 after the TTL expired", hits)
	}
}

func TestKnowledgeFetcher200ClearsDarkMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"files":[]}`))
	}))
	defer srv.Close()

	f := newKnowledgeFetcher(srv.Client(), srv.URL, "tok", "ws1")
	f.markerPath = filepath.Join(t.TempDir(), "dark-marker")
	old := time.Now().Add(-knowledgeDarkTTL - time.Minute)
	if err := os.WriteFile(f.markerPath, []byte("dark"), 0o600); err != nil {
		t.Fatalf("seed marker: %v", err)
	}
	if err := os.Chtimes(f.markerPath, old, old); err != nil {
		t.Fatalf("age marker: %v", err)
	}

	if _, err := f.fetch(context.Background()); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if _, err := os.Stat(f.markerPath); !os.IsNotExist(err) {
		t.Fatalf("marker still present after a 200 — a live feature must not stay negatively cached")
	}
}
