package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/adr"
	"github.com/lemahq/lema-mcp/internal/source"
)

// resolve is a router, not a re-implementation: each intent must reach the SAME
// handler its standalone alias uses, and the envelope must carry exactly that
// handler's output under the matching field. These tests pin the routing (the
// alias-then-deprecate guarantee) and the honest-note paths — never a fabricated
// answer when a mode cannot serve.

// setLocalRecord wires src+capture to a small closed corpus, mirroring the
// check_decided/search parity fixtures (several closures so the distinctiveness
// matcher has IDF to work with).
func setLocalRecord(t *testing.T) {
	t.Helper()
	oldSrc, oldCapture, oldRepo := src, capture, repoName
	t.Cleanup(func() { src, capture, repoName = oldSrc, oldCapture, oldRepo })

	cs, err := source.NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatalf("NewCaptureStore: %v", err)
	}
	capture = cs
	src = source.NewLocal([]adr.ADR{
		{Number: 7, Title: "public API protocol", Status: "accepted",
			Body: "## Alternatives considered\n\n### GraphQL\n- **Status:** rejected — resolver N+1 cost and cache opacity\n"},
		{Number: 8, Title: "primary datastore", Status: "accepted",
			Body: "## Context\nThe audit log needs ACID writes.\n\n## Decision\nWe chose PostgreSQL.\n\n## Alternatives considered\n\n### MongoDB\n- **Status:** rejected — audit trail needs strict consistency\n"},
		{Number: 9, Title: "event transport", Status: "accepted",
			Body: "## Alternatives considered\n\n### Kafka\n- **Status:** rejected — ops burden\n"},
	})
	repoName = "test/repo"
}

func TestResolveApproachRoutesToCheckDecided(t *testing.T) {
	setLocalRecord(t)
	_, out, err := resolve(context.Background(), nil, resolveInput{
		Intent: "approach", Approach: "adopt GraphQL for the public API",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Intent != "approach" || out.Approach == nil {
		t.Fatalf("approach-mode must populate the Approach payload: %+v", out)
	}
	if !out.Approach.Decided || out.Approach.Verdict == "" {
		t.Fatalf("resolve must pass check_decided's verdict through verbatim: %+v", out.Approach)
	}
	if out.Why != nil || out.Record != nil || out.Search != nil {
		t.Fatal("exactly one mode payload may be set")
	}
}

// The approach field is the documented input, but a caller that puts the option
// in query (the why/id field) should still be adjudicated — resolve falls back.
func TestResolveApproachFallsBackToQuery(t *testing.T) {
	setLocalRecord(t)
	_, out, err := resolve(context.Background(), nil, resolveInput{
		Intent: "approach", Query: "adopt GraphQL for the public API",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Approach == nil || !out.Approach.Decided {
		t.Fatalf("approach-mode must fall back to query when approach is empty: %+v", out)
	}
}

func TestResolveIDByNumberRoutesToGetDecision(t *testing.T) {
	setLocalRecord(t)
	_, out, err := resolve(context.Background(), nil, resolveInput{Intent: "id", Number: 8})
	if err != nil {
		t.Fatal(err)
	}
	if out.Record == nil || out.Record.Decision.Number != 8 {
		t.Fatalf("id-mode by number must return decision #8's detail: %+v", out)
	}
	if out.Search != nil {
		t.Fatal("a numbered id-read is a detail fetch, not a search")
	}
}

func TestResolveIDByQueryRoutesToSearch(t *testing.T) {
	setLocalRecord(t)
	_, out, err := resolve(context.Background(), nil, resolveInput{Intent: "id", Query: "postgres audit log"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Search == nil || len(out.Search.Claims) == 0 {
		t.Fatalf("id-mode by query must return ranked claims: %+v", out)
	}
	if out.Record != nil {
		t.Fatal("a query id-read is a search, not a detail fetch")
	}
}

func TestResolveWhyHostedOnlyNoteWithoutHosted(t *testing.T) {
	old := hostedSrc
	t.Cleanup(func() { hostedSrc = old })
	hostedSrc = nil

	_, out, err := resolve(context.Background(), nil, resolveInput{Intent: "why", Query: "why postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Why != nil {
		t.Fatal("why-mode without hosted config must not fabricate an answer")
	}
	if !strings.Contains(out.Note, "hosted-only") {
		t.Fatalf("why-mode must name the hosted-only requirement honestly: %q", out.Note)
	}
}

func TestResolveWhyRoutesToAskWhenHosted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ask" {
			t.Errorf("why-mode must POST /ask, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"scope":  "workspace acme",
			"answer": "Postgres was chosen for ACID audit writes [1].",
			"sources": []map[string]any{
				{"n": 1, "ref": "ADR-0008", "type": "chosen", "text": "chose PostgreSQL"},
			},
			"usage": map[string]any{"atoms_tokens": 10, "source_tokens": 40},
		})
	}))
	defer ts.Close()
	old := hostedSrc
	oldRuntime, oldProvider := processHostedRuntime, processTargetProvider
	t.Cleanup(func() {
		hostedSrc = old
		processHostedRuntime, processTargetProvider = oldRuntime, oldProvider
	})
	hostedSrc = source.NewHosted(ts.URL, "tok", ts.Client())
	runtime := hostedWriteRuntime{
		hosted:  hostedSrc,
		targets: &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: projectReadContext()}},
	}
	processHostedRuntime = &runtime
	processTargetProvider = runtime.targets

	_, out, err := resolve(context.Background(), nil, resolveInput{Intent: "why", Query: "why postgres"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Why == nil || !strings.Contains(out.Why.Answer, "Postgres") {
		t.Fatalf("why-mode must route to ask and return the synthesized answer: %+v", out)
	}
	if len(out.Why.Sources) != 1 {
		t.Fatalf("cited sources must ride through: %+v", out.Why)
	}
}

func TestResolveUnknownIntentHonestNote(t *testing.T) {
	_, out, err := resolve(context.Background(), nil, resolveInput{Intent: "explain", Query: "anything"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Why != nil || out.Approach != nil || out.Record != nil || out.Search != nil {
		t.Fatal("an unknown intent must return no payload")
	}
	if !strings.Contains(out.Note, "why") || !strings.Contains(out.Note, "approach") || !strings.Contains(out.Note, "id") {
		t.Fatalf("unknown intent must name the valid intents: %q", out.Note)
	}
}
