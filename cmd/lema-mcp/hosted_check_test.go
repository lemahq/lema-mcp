package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// These tests pin the D.1 hosted check_decided leg end to end on the client:
// in hosted mode (src = *source.Hosted), check_decided pulls the org's CLOSED
// set from GET /closed-atoms and enforces it through the same weighted
// matcher as local capture. Before this leg, the tool SILENTLY checked local
// capture only — the type assertion to ClosedSource failed for Hosted and the
// hosted record was never consulted. That silent gap is the bug; the error
// test below is the guarantee it can't come back quietly.

func swapHostedGlobals(t *testing.T, hosted *source.Hosted) {
	t.Helper()
	oldSrc, oldCapture := src, capture
	t.Cleanup(func() { src, capture = oldSrc, oldCapture })
	src = hosted
	cs, err := source.NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatalf("NewCaptureStore: %v", err)
	}
	capture = cs
}

func TestCheckDecidedHostedReturnsHostedClosures(t *testing.T) {
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/closed-atoms" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		hits++
		// A realistic org no-go set: several closures, so the matcher's
		// corpus IDF has distribution to work with (a single-entry corpus
		// flattens every weight to 1.0, below the 1.5 threshold — by design:
		// distinctiveness is relative).
		_ = json.NewEncoder(w).Encode(map[string]any{
			"atoms": []map[string]any{
				{
					"id": "c1", "type": "rejected_alternative",
					"text":        "Kafka — rejected: ops burden too high",
					"ref":         "ADR-0012",
					"closed":      true,
					"closed_note": `do not propose "Kafka": ops burden too high (ADR-0012 · "Event transport")`,
					"match_key":   "Kafka",
				},
				{
					"id": "c2", "type": "rejected_alternative",
					"text":        "MongoDB — rejected: eventual consistency breaks the audit trail",
					"ref":         "ADR-0008",
					"closed":      true,
					"closed_note": "do not propose MongoDB",
					"match_key":   "MongoDB",
				},
				{
					"id": "c3", "type": "rejected_alternative",
					"text":        "client-side rendering — rejected: SEO requirements",
					"ref":         "ADR-0019",
					"closed":      true,
					"closed_note": "do not propose client-side rendering",
					"match_key":   "client-side rendering",
				},
			},
		})
	}))
	defer ts.Close()
	swapHostedGlobals(t, source.NewHosted(ts.URL, "lema_live_x", ts.Client()))

	_, out, err := checkDecided(context.Background(), nil, checkInput{Topic: "should we adopt Kafka for the event bus?"})
	if err != nil {
		t.Fatalf("checkDecided: %v", err)
	}
	if hits != 1 {
		t.Errorf("hosted /closed-atoms hit %d times, want 1", hits)
	}
	if !out.Decided || len(out.Closed) != 1 {
		t.Fatalf("out = %+v, want Decided with the hosted Kafka closure", out)
	}
	if out.Closed[0].Ref != "ADR-0012" || !out.Closed[0].Closed {
		t.Errorf("closure lost provenance: %+v", out.Closed[0])
	}
	if out.Note == "" {
		t.Error("CLOSED verdict carries no enforcement note")
	}
}

func TestCheckDecidedHostedNoMatchStaysOpen(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"atoms": []map[string]any{
				{"id": "c1", "type": "rejected_alternative", "text": "Kafka — rejected: x",
					"ref": "ADR-0012", "closed": true, "closed_note": "do not propose Kafka", "match_key": "Kafka"},
			},
		})
	}))
	defer ts.Close()
	swapHostedGlobals(t, source.NewHosted(ts.URL, "lema_live_x", ts.Client()))

	_, out, err := checkDecided(context.Background(), nil, checkInput{Topic: "which CSS framework should we use?"})
	if err != nil {
		t.Fatalf("checkDecided: %v", err)
	}
	if out.Decided {
		t.Errorf("unrelated topic came back Decided: %+v", out.Closed)
	}
}

// TestCheckDecidedHostedFetchFailureFailsLoud is the anti-regression for the
// original bug: when the hosted fetch fails, the tool must ERROR — a silent
// fall-back to local-only checking would tell the agent "not decided" about
// options the team has closed, which is exactly what D.1 exists to fix.
func TestCheckDecidedHostedFetchFailureFailsLoud(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	swapHostedGlobals(t, source.NewHosted(ts.URL, "lema_live_x", ts.Client()))

	_, _, err := checkDecided(context.Background(), nil, checkInput{Topic: "adopt Kafka?"})
	if err == nil {
		t.Fatal("checkDecided returned nil error on hosted fetch failure — silent local-only degrade")
	}
	if !strings.Contains(err.Error(), "hosted") {
		t.Errorf("error %q does not name the hosted leg", err)
	}
}

// TestCheckDecidedHostedMergesLocalCapture: hosted closures and the local
// capture file enforce together — a locally captured rejection still fires in
// hosted mode (capture is mode-independent, ADR-0042).
func TestCheckDecidedHostedMergesLocalCapture(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"atoms": []map[string]any{}})
	}))
	defer ts.Close()
	swapHostedGlobals(t, source.NewHosted(ts.URL, "lema_live_x", ts.Client()))

	if _, err := capture.Record(source.DecisionRecord{
		Title:  "frontend data fetching",
		Chosen: "SWR",
		Rejected: []source.RejectedAlt{
			{Option: "tanstack-query", Why: "bundle size for our case"},
		},
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	_, out, err := checkDecided(context.Background(), nil, checkInput{Topic: "use tanstack-query for data fetching"})
	if err != nil {
		t.Fatalf("checkDecided: %v", err)
	}
	if !out.Decided {
		t.Error("locally captured rejection did not fire in hosted mode")
	}
}
