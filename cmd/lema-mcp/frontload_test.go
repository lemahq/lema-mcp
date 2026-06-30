package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// Task A — the dark-by-default flag gate, read per invocation (mirrors pushEnabled).
func TestFrontloadEnabled(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "yes": false, "1": true, "true": true} {
		t.Setenv("LEMA_FUSE_FRONTLOAD", v)
		if got := frontloadEnabled(); got != want {
			t.Errorf("LEMA_FUSE_FRONTLOAD=%q: frontloadEnabled()=%v, want %v", v, got, want)
		}
	}
}

// Task B — the scope-query builder: the prompt text IS the query (UserPromptSubmit
// primary). A deterministic transform (code, not the model).
func TestBuildScopeQuery(t *testing.T) {
	t.Run("UserPromptSubmit prompt becomes the query, trimmed", func(t *testing.T) {
		got := buildScopeQuery(frontloadInput{Prompt: "  Add rate limiting to the login endpoint  "})
		if got != "Add rate limiting to the login endpoint" {
			t.Errorf("buildScopeQuery = %q", got)
		}
	})
	t.Run("blank prompt yields an empty query so the caller no-ops", func(t *testing.T) {
		if got := buildScopeQuery(frontloadInput{Prompt: "   "}); got != "" {
			t.Errorf("buildScopeQuery(blank) = %q, want empty", got)
		}
	})
	t.Run("an over-long prompt is capped to the query-length limit", func(t *testing.T) {
		long := strings.Repeat("x", frontloadMaxQueryLen+500)
		if got := buildScopeQuery(frontloadInput{Prompt: long}); len([]rune(got)) != frontloadMaxQueryLen {
			t.Errorf("capped len = %d, want %d", len([]rune(got)), frontloadMaxQueryLen)
		}
	})
}

// Task C — the testable runner core (fakes injected, mirrors pushRunner). Every
// failure path is a fail-open no-op; abstain (no sources) injects zero bytes.
func TestFrontloadRunner(t *testing.T) {
	ctx := context.Background()
	promptIn := frontloadInput{Prompt: "Add rate limiting to the login endpoint"}

	withSources := func(context.Context, string) (source.AskResult, error) {
		return source.AskResult{
			Answer: "Auth middleware must stay provider-agnostic [1]. Rate limit in the API middleware, not the login page [2].",
			Sources: []source.AskSource{
				{N: 1, Ref: "ADR-014", Type: "decision"},
				{N: 2, Ref: "PR-284", Type: "decision"},
			},
		}, nil
	}

	t.Run("no creds/workspace resolved injects nothing and never queries", func(t *testing.T) {
		queried := false
		r := frontloadRunner{ask: func(context.Context, string) (source.AskResult, error) {
			queried = true
			return source.AskResult{}, nil
		}, canQuery: false}
		if out := r.run(ctx, promptIn); out != "" || queried {
			t.Errorf("out=%q queried=%v; want empty + no query", out, queried)
		}
	})

	t.Run("an empty scope query injects nothing and never queries", func(t *testing.T) {
		queried := false
		r := frontloadRunner{ask: func(context.Context, string) (source.AskResult, error) {
			queried = true
			return source.AskResult{}, nil
		}, canQuery: true}
		if out := r.run(ctx, frontloadInput{Prompt: "   "}); out != "" || queried {
			t.Errorf("out=%q queried=%v; want empty + no query", out, queried)
		}
	})

	t.Run("cited sources are injected as the cited-why block", func(t *testing.T) {
		r := frontloadRunner{ask: withSources, canQuery: true}
		out := r.run(ctx, promptIn)
		if out == "" {
			t.Fatal("expected a non-empty frontload block")
		}
		for _, want := range []string{"ADR-014", "provider-agnostic"} {
			if !strings.Contains(out, want) {
				t.Errorf("frontload block missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("abstain (no sources) injects zero bytes — silence is the honest answer", func(t *testing.T) {
		abstain := func(context.Context, string) (source.AskResult, error) {
			return source.AskResult{Answer: "No decision is recorded that answers this.", Sources: nil}, nil
		}
		r := frontloadRunner{ask: abstain, canQuery: true}
		if out := r.run(ctx, promptIn); out != "" {
			t.Errorf("on abstain want zero bytes, got %q", out)
		}
	})

	t.Run("an ask error fails open and injects nothing", func(t *testing.T) {
		boom := func(context.Context, string) (source.AskResult, error) { return source.AskResult{}, errors.New("boom") }
		r := frontloadRunner{ask: boom, canQuery: true}
		if out := r.run(ctx, promptIn); out != "" {
			t.Errorf("on error want zero bytes, got %q", out)
		}
	})

	t.Run("sources beyond the cap are dropped — never the whole graph", func(t *testing.T) {
		many := func(context.Context, string) (source.AskResult, error) {
			srcs := make([]source.AskSource, frontloadMaxSources+3)
			for i := range srcs {
				srcs[i] = source.AskSource{N: i + 1, Ref: fmt.Sprintf("D-%d", i+1), Type: "decision"}
			}
			return source.AskResult{Answer: "ans", Sources: srcs}, nil
		}
		r := frontloadRunner{ask: many, canQuery: true}
		out := r.run(ctx, promptIn)
		if !strings.Contains(out, "D-1") {
			t.Errorf("expected the top source D-1 in output:\n%s", out)
		}
		if strings.Contains(out, fmt.Sprintf("D-%d", frontloadMaxSources+1)) {
			t.Errorf("a source beyond the cap leaked into output:\n%s", out)
		}
	})
}

// Task D — the stdout render: cited block when there's content, zero bytes otherwise.
func TestRenderFrontload(t *testing.T) {
	t.Run("renders the answer and each cited source", func(t *testing.T) {
		out := renderFrontload("Use Clerk, not Auth0 [1].", []source.AskSource{
			{N: 1, Ref: "ADR-017", Type: "decision", Locator: "docs/adr/0017.md"},
		})
		for _, want := range []string{"Use Clerk", "ADR-017", "docs/adr/0017.md"} {
			if !strings.Contains(out, want) {
				t.Errorf("render missing %q:\n%s", want, out)
			}
		}
	})
	t.Run("empty answer and no sources renders zero bytes", func(t *testing.T) {
		if out := renderFrontload("", nil); out != "" {
			t.Errorf("want zero bytes, got %q", out)
		}
	})
}
