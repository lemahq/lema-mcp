package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lemahq/lema-mcp/internal/adr"
	"github.com/lemahq/lema-mcp/internal/source"
)

// The GUI /api/check endpoint (serve.go) and the stdio check_decided MCP tool
// are two doors to the SAME never-reopen judgment — a cockpit rendering
// /api/check must show exactly what an agent calling check_decided sees. The
// lema-terminal design debate caught them disagreeing on the same topic (its
// design-lock ruling: "one adjudicator, one matcher, at the boundary"):
// /api/check ran capture.CheckDecided — a local-lexical filter over the capture
// store only, with no repo-ADR set, no hosted org closures, and no ADR-0094
// verdict envelope — while check_decided judged the full merged set through
// verdict.Build. So the GUI could render "not decided" (and no verdict at all)
// for an option the org has closed. These tests pin the parity on every axis a
// consumer can key off: decided, the closed set, verdict, governing decisions.
// They fail the moment either surface grows its own acquisition or judgment.

// callHTTPCheck drives the /api/check handler exactly as the GUI does and
// decodes the response into the shared checkOutput shape.
func callHTTPCheck(t *testing.T, topic string) (int, checkOutput) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/check?topic="+urlQueryEscape(topic), nil)
	rec := httptest.NewRecorder()
	httpCheck(rec, req)
	var out checkOutput
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("/api/check response is not a checkOutput: %v (body %q)", err, rec.Body.String())
	}
	return rec.Code, out
}

func urlQueryEscape(s string) string {
	// net/url via the request parser: build with a URL-safe query value.
	req := httptest.NewRequest(http.MethodGet, "/api/check", nil)
	q := req.URL.Query()
	q.Set("topic", s)
	return q.Encode()[len("topic="):]
}

// asWire round-trips the tool output through JSON, because that is what every
// consumer receives on BOTH surfaces (the MCP transport serializes the tool
// result the same way /api/check does). Non-serialized matcher internals
// (Score, MatchKey — json:"-") are not part of the parity contract.
func asWire(t *testing.T, out checkOutput) checkOutput {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal tool output: %v", err)
	}
	var wire checkOutput
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal tool output: %v", err)
	}
	return wire
}

// Hosted mode: both surfaces must return the identical judgment over the
// identical merged set — org closures (hosted /closed-atoms) plus the local
// capture store — including the verdict envelope.
func TestHTTPCheckMatchesCheckDecidedToolHosted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	topic := "should we adopt Kafka for the event bus?"
	_, toolOut, err := checkDecided(context.Background(), nil, checkInput{Topic: topic})
	if err != nil {
		t.Fatalf("checkDecided: %v", err)
	}
	if !toolOut.Decided || toolOut.Verdict == "" {
		t.Fatalf("fixture must produce a decided verdict through the tool, got %+v", toolOut)
	}

	code, httpOut := callHTTPCheck(t, topic)
	if code != http.StatusOK {
		t.Fatalf("/api/check status = %d, want 200", code)
	}
	if !reflect.DeepEqual(asWire(t, toolOut), httpOut) {
		t.Errorf("/api/check diverges from check_decided for the same topic:\n tool: %+v\n http: %+v", toolOut, httpOut)
	}
	if httpOut.Verdict == "" {
		t.Error("/api/check returned no verdict envelope — a consumer keying off verdict reads neither ruled_out nor not_ruled_out")
	}
}

// Local mode: the tool enforces off the capture store AND the repo's documented
// ADR closures (ADR-0053). /api/check must consult the same merged set — before
// the fix it read the capture store only, so a decision recorded in an ADR (but
// never captured live) fired in check_decided and stayed silent in the GUI.
func TestHTTPCheckMatchesCheckDecidedToolLocal(t *testing.T) {
	oldSrc, oldCapture := src, capture
	t.Cleanup(func() { src, capture = oldSrc, oldCapture })

	cs, err := source.NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatalf("NewCaptureStore: %v", err)
	}
	capture = cs
	// Several closures so the distinctiveness matcher has corpus IDF to work
	// with (a single-entry corpus flattens every weight below the threshold).
	src = source.NewLocal([]adr.ADR{
		{Number: 7, Title: "public API protocol", Status: "accepted",
			Body: "## Alternatives considered\n\n### GraphQL\n- **Status:** rejected — resolver N+1 cost and cache opacity\n"},
		{Number: 8, Title: "event transport", Status: "accepted",
			Body: "## Alternatives considered\n\n### Kafka\n- **Status:** rejected — ops burden\n"},
		{Number: 9, Title: "primary datastore", Status: "accepted",
			Body: "## Alternatives considered\n\n### MongoDB\n- **Status:** rejected — audit trail needs strict consistency\n"},
	})

	topic := "adopt GraphQL for the public API"
	_, toolOut, err := checkDecided(context.Background(), nil, checkInput{Topic: topic})
	if err != nil {
		t.Fatalf("checkDecided: %v", err)
	}
	if !toolOut.Decided {
		t.Fatalf("ADR closure did not fire through the tool — fixture broken: %+v", toolOut)
	}

	code, httpOut := callHTTPCheck(t, topic)
	if code != http.StatusOK {
		t.Fatalf("/api/check status = %d, want 200", code)
	}
	if !reflect.DeepEqual(asWire(t, toolOut), httpOut) {
		t.Errorf("/api/check diverges from check_decided for the same topic:\n tool: %+v\n http: %+v", toolOut, httpOut)
	}
}

// A hosted fetch failure must fail LOUD on the HTTP surface exactly as the tool
// does (ADR-0094): a non-200 with the errored envelope — never a confident
// 200 "not decided" computed from local capture alone.
func TestHTTPCheckHostedFetchFailureFailsLoud(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	swapHostedGlobals(t, source.NewHosted(ts.URL, "lema_live_x", ts.Client()))

	code, out := callHTTPCheck(t, "adopt Kafka?")
	if code == http.StatusOK {
		t.Fatalf("/api/check returned 200 on hosted fetch failure — silent local-only degrade (body verdict %q)", out.Verdict)
	}
	if out.Verdict != "error" {
		t.Errorf("verdict = %q, want the errored envelope so the GUI renders a retryable failure, not an answer", out.Verdict)
	}
	if out.Decided {
		t.Error("Decided = true on a failed fetch — must never claim a judgment it could not load")
	}
}
