package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// TestCheckApproachHostedDescriptionDirectoryClean guards the hosted-mode
// description (#293): it is shipped to a Directory-reviewed host like its public
// sibling, so it must describe what the tool returns and never instruct the agent
// how to behave. The count-based TestToolsMeetDirectoryCriteria can't cover it (a
// second check_approach would break the tool count), so this pins it directly.
func TestCheckApproachHostedDescriptionDirectoryClean(t *testing.T) {
	banned := []string{"call this", "you must", "do not re-propose", "prefer this over", "instead of reading", "before you propose", "before you write"}
	low := strings.ToLower(checkApproachHostedDescription)
	for _, b := range banned {
		if strings.Contains(low, b) {
			t.Errorf("hosted description contains banned steering phrase %q", b)
		}
	}
	if checkApproachHostedDescription == "" {
		t.Fatal("hosted description is empty")
	}
	// The authed handler (checkApproachAuthed) emits only ruled_out / no_recorded_ruling
	// — it has no `settled` rung — so the hosted description must not promise `settled`.
	if strings.Contains(strings.ToLower(checkApproachHostedDescription), "settled") {
		t.Error("hosted description promises 'settled', but the authed handler never emits it")
	}
}

// swapHostedSrc points the hosted client at a test server for the duration of a
// test and restores it after. check_approach's hosted leg (#293) routes through
// hostedSrc, so this is how we drive the own-corpus path without a live API.
func swapHostedSrc(t *testing.T, h *source.Hosted) {
	t.Helper()
	old := hostedSrc
	oldRuntime, oldProvider := processHostedRuntime, processTargetProvider
	t.Cleanup(func() {
		hostedSrc = old
		processHostedRuntime, processTargetProvider = oldRuntime, oldProvider
	})
	hostedSrc = h
	runtime := hostedWriteRuntime{
		hosted:  h,
		targets: &fakeTargetProvider{result: resolutionResult{Status: resolutionResolved, Context: projectReadContext()}},
	}
	processHostedRuntime = &runtime
	processTargetProvider = runtime.targets
}

// TestCheckApproachHostedAnswersOwnCorpus pins the #293 MCP routing: in hosted mode
// (LEMA_API_URL set → hostedSrc non-nil), check_approach answers over the caller's
// OWN corpus via the authed POST /check-approach with the bearer token — NOT the
// public commons. The repo arg is a public-commons selector and is moot here; the
// tool must still reach the own-corpus endpoint and map its ruled_out verdict.
func TestCheckApproachHostedAnswersOwnCorpus(t *testing.T) {
	var gotPath, gotAuth, gotApproach string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotApproach, _ = req["approach"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "ws-acme", "approach": "use Kafka for the event bus", "verdict": "ruled_out",
			"why_not": "Kafka was rejected for ops burden [1].",
			"sources": []map[string]any{{"n": 1, "ref": "ADR-0012", "type": "rejected", "text": "Kafka rejected: ops burden"}},
			"note":    "this approach touches a recorded rejection",
		})
	}))
	defer ts.Close()
	swapHostedSrc(t, source.NewHosted(ts.URL, "lema_live_x", ts.Client()))

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "use Kafka for the event bus")
	if err != nil {
		t.Fatalf("runCheckApproach: %v", err)
	}
	if gotPath != "/check-approach" {
		t.Errorf("path = %q, want /check-approach — hosted mode must hit the authed own-corpus endpoint, not /fuse", gotPath)
	}
	if gotAuth != "Bearer lema_live_x" {
		t.Errorf("auth = %q, want the bearer token", gotAuth)
	}
	if gotApproach != "use Kafka for the event bus" {
		t.Errorf("forwarded approach = %q", gotApproach)
	}
	if out.Verdict != "ruled_out" {
		t.Fatalf("verdict = %q, want ruled_out", out.Verdict)
	}
	if len(out.Sources) != 1 || out.Sources[0].Ref != "ADR-0012" {
		t.Fatalf("own-corpus ruling lost its citation: %+v", out.Sources)
	}
	if out.WhyNot == "" {
		t.Error("a grounded ruled_out must carry the why-not")
	}
	// The own-corpus leg already annotated edges + queried the connected repo, so the
	// public cold-import caveats ("isn't tracked in the public graph") would LIE here.
	for _, c := range out.Caveats {
		if strings.Contains(strings.ToLower(c), "public graph") {
			t.Errorf("own-corpus ruling must not carry the public-import caveat %q", c)
		}
	}
	if out.GroundingNote == "" {
		t.Error("a grounded ruling must still carry the (corpus-neutral) grounding steer")
	}
}

// TestCheckApproachHostedAbstainMaps pins that an own-corpus no_recorded_ruling maps
// through the same honest abstain path (coverage + connect-CTA), so the hosted leg
// reuses the public mapping rather than a divergent one.
func TestCheckApproachHostedAbstainMaps(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "all 2 workspaces", "approach": "x", "verdict": "no_recorded_ruling", "sources": []any{},
		})
	}))
	defer ts.Close()
	swapHostedSrc(t, source.NewHosted(ts.URL, "tok", ts.Client()))

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "an unrecorded approach")
	if err != nil {
		t.Fatalf("runCheckApproach: %v", err)
	}
	if out.Verdict != "no_recorded_ruling" {
		t.Fatalf("verdict = %q, want no_recorded_ruling", out.Verdict)
	}
	if out.Coverage == nil || out.Coverage.Sufficient {
		t.Errorf("an abstain must carry a non-sufficient coverage slice, got %+v", out.Coverage)
	}
	// The caller's repo IS already connected, so the public "connect your repo" CTA and
	// the "public corpus" coverage note are both wrong on the own-corpus leg.
	if out.Upgrade != "" {
		t.Errorf("own-corpus abstain must not nag to connect an already-connected repo, got Upgrade=%q", out.Upgrade)
	}
	if out.Coverage != nil && strings.Contains(strings.ToLower(out.Coverage.Note), "public corpus") {
		t.Errorf("own-corpus coverage note must not misname the corpus as 'public corpus', got %q", out.Coverage.Note)
	}
}

// TestCheckApproachHostedQuotaDegradesGracefully pins that a 429 from the authed
// /check-approach (the plan query quota) renders an honest "limit reached" note to
// the paying user — not a raw tool error, which is what the public path's
// ErrPublicRateLimited mapping guarantees for the tokenless leg.
func TestCheckApproachHostedQuotaDegradesGracefully(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "daily_limit_reached"})
	}))
	defer ts.Close()
	swapHostedSrc(t, source.NewHosted(ts.URL, "tok", ts.Client()))

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "anything")
	if err != nil {
		t.Fatalf("a quota 429 must degrade to an honest note, not a tool error: %v", err)
	}
	if out.Note == "" || !strings.Contains(strings.ToLower(out.Note), "limit") {
		t.Errorf("quota degrade must carry a 'limit reached' note, got %q", out.Note)
	}
}
