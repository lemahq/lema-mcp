package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
)

// guardTestServer wires the real interception routes (the #328/#329 server half) so
// terminal-mode is exercised against the actual endpoints the hook calls in
// production — no mocks, the genuine POST / poll / resolve loop.
func guardTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/guard", httpGuard)
	mux.HandleFunc("/api/guard/pending", httpGuardPending)
	mux.HandleFunc("/api/guard/resolve", httpGuardResolve)
	mux.HandleFunc("/api/guard/result", httpGuardResult)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// fastGuardPoll shrinks the terminal-mode poll interval so the block/resolve loop
// spins in milliseconds under test; restored on cleanup.
func fastGuardPoll(t *testing.T) {
	t.Helper()
	old := guardPollInterval
	guardPollInterval = 2 * time.Millisecond
	t.Cleanup(func() { guardPollInterval = old })
}

// resolveFirstPending stands in for the human at the terminal: it watches the open
// interception list and resolves the first one that appears while the hook blocks on
// its poll.
func resolveFirstPending(ts *httptest.Server, resolution, why string) {
	go func() {
		for i := 0; i < 500; i++ {
			resp, err := ts.Client().Get(ts.URL + "/api/guard/pending")
			if err == nil {
				var listed struct {
					Pending []guardPending `json:"pending"`
				}
				_ = json.NewDecoder(resp.Body).Decode(&listed)
				resp.Body.Close()
				if len(listed.Pending) > 0 {
					body, _ := json.Marshal(map[string]string{
						"id": listed.Pending[0].ID, "resolution": resolution, "why": why,
					})
					if r, err := ts.Client().Post(ts.URL+"/api/guard/resolve", "application/json", bytes.NewReader(body)); err == nil {
						r.Body.Close()
					}
					return
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

func kafkaEdit() guardInput {
	return guardInput{ToolName: "Edit", ToolInput: map[string]any{
		"file_path": "q.go", "new_string": "new KafkaClient()",
	}}
}

// mapResolution is the pure mapping from a human's terminal resolution to a
// PreToolUse decision: :respect binds a human-bound deny (the invariant's
// interactive-human discard) that CITES the settled decision; :override and any
// unknown/empty resolution proceed silently (the hook never blocks on its own).
func TestMapResolution(t *testing.T) {
	closed := []source.Atom{{
		ClosedNote: `do not propose "Kafka": operational burden for our scale`,
		Text:       "Kafka — operational burden for our scale",
	}}

	respect := mapResolution("respect", closed)
	if respect == nil {
		t.Fatal("respect must bind a decision, got nil (proceed)")
	}
	if respect.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("respect must map to deny, got %q", respect.HookSpecificOutput.PermissionDecision)
	}
	if !strings.Contains(respect.HookSpecificOutput.PermissionDecisionReason, "Kafka") {
		t.Fatalf("the deny reason must cite the settled decision, got %q", respect.HookSpecificOutput.PermissionDecisionReason)
	}

	if got := mapResolution("override", closed); got != nil {
		t.Fatalf("override must proceed silently (nil), got %+v", got)
	}
	if got := mapResolution("", closed); got != nil {
		t.Fatalf("empty resolution must proceed silently (nil), got %+v", got)
	}
	if got := mapResolution("garbage", closed); got != nil {
		t.Fatalf("unknown resolution must proceed silently (nil), got %+v", got)
	}
}

// End to end: the hook POSTs a tool-call that reaches a settled decision, blocks, the
// human presses :respect at the terminal, and the hook returns a deny that cites the
// decision. The deny is legitimate because a human bound it — the matcher never
// denies on its own.
func TestGuardViaTerminalRespectBindsDeny(t *testing.T) {
	oldCapture := capture
	capture = newTestStore(t)
	guardPendings = newGuardPendingStore()
	t.Cleanup(func() { capture = oldCapture; guardPendings = newGuardPendingStore() })
	fastGuardPoll(t)
	ts := guardTestServer(t)

	resolveFirstPending(ts, "respect", "")
	out := guardViaTerminal(ts.Client(), ts.URL, kafkaEdit())
	if out == nil {
		t.Fatal("a respected interception must produce a deny, got nil (proceed)")
	}
	if out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("respect must bind a deny, got %q", out.HookSpecificOutput.PermissionDecision)
	}
	if !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "Kafka") {
		t.Fatalf("the deny reason must cite the settled decision, got %q", out.HookSpecificOutput.PermissionDecisionReason)
	}
}

// End to end: same interception, but the human presses :override — the hook proceeds
// silently (nil). The terminal owns the superseding record_decision; the hook just
// stops blocking.
func TestGuardViaTerminalOverrideProceeds(t *testing.T) {
	oldCapture := capture
	capture = newTestStore(t)
	guardPendings = newGuardPendingStore()
	t.Cleanup(func() { capture = oldCapture; guardPendings = newGuardPendingStore() })
	fastGuardPoll(t)
	ts := guardTestServer(t)

	resolveFirstPending(ts, "override", "adopting Kafka deliberately for the new streaming tier")
	if out := guardViaTerminal(ts.Client(), ts.URL, kafkaEdit()); out != nil {
		t.Fatalf("override must proceed silently, got %+v", out)
	}
}

// When the tool-call reaches no settled decision, the POST returns decided=false and
// the hook proceeds immediately — no pending is opened, no poll.
func TestGuardViaTerminalProceedsWhenNothingDecided(t *testing.T) {
	oldCapture := capture
	capture = newTestStore(t)
	guardPendings = newGuardPendingStore()
	t.Cleanup(func() { capture = oldCapture; guardPendings = newGuardPendingStore() })
	ts := guardTestServer(t)

	benign := guardInput{ToolName: "Edit", ToolInput: map[string]any{
		"file_path": "readme.md", "new_string": "hello world, nothing settled here",
	}}
	if out := guardViaTerminal(ts.Client(), ts.URL, benign); out != nil {
		t.Fatalf("a call reaching no settled decision must proceed, got %+v", out)
	}
	if len(guardPendings.open()) != 0 {
		t.Fatalf("no pending must be opened when nothing is decided, got %+v", guardPendings.open())
	}
}

// The whole hook seam in terminal mode: with LEMA_GUARD_ENDPOINT set, runGuard reads
// the PreToolUse payload from stdin, delegates to the sidecar, blocks for the human's
// :respect, and writes a deny to stdout. Asserting "deny" distinguishes terminal mode
// from the local path, which only ever emits additionalContext.
func TestRunGuardTerminalModeEndToEnd(t *testing.T) {
	oldCapture := capture
	capture = newTestStore(t)
	guardPendings = newGuardPendingStore()
	t.Cleanup(func() { capture = oldCapture; guardPendings = newGuardPendingStore() })
	fastGuardPoll(t)
	ts := guardTestServer(t)
	t.Setenv(guardEndpointEnvVar, ts.URL)

	resolveFirstPending(ts, "respect", "")
	stdin := `{"tool_name":"Edit","tool_input":{"file_path":"q.go","old_string":"x","new_string":"new KafkaClient()"}}`
	out := captureRunGuard(t, stdin, nil)
	if !strings.Contains(out, `"permissionDecision":"deny"`) {
		t.Fatalf("terminal mode + :respect must emit a deny, got: %q", out)
	}
	if !strings.Contains(out, "Kafka") {
		t.Fatalf("the deny must cite the settled decision, got: %q", out)
	}
}

// The sidecar being unreachable must fail open — an advisory layer never wedges the
// agent on its own infrastructure.
func TestGuardViaTerminalFailsOpenOnTransportError(t *testing.T) {
	dead := httptest.NewServer(http.NewServeMux())
	url := dead.URL
	client := dead.Client()
	dead.Close() // now nothing is listening on url

	if out := guardViaTerminal(client, url, kafkaEdit()); out != nil {
		t.Fatalf("an unreachable sidecar must fail open (nil), got %+v", out)
	}
}
