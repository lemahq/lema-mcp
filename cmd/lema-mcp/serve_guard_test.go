package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// POST /api/guard with a tool-call that reaches for a ruled-out option returns the
// cited CLOSED decision (advisory) — the interception the terminal renders at the
// tool-call boundary. This is the engine half of the join: tool-call -> the same
// guard match the PreToolUse hook uses -> the rejected alternative + its recorded
// why, rendered (never computed) by the terminal.
func TestHTTPGuardReturnsClosedDecision(t *testing.T) {
	oldCapture := capture
	capture = newTestStore(t) // records a CLOSED "Kafka" rejected alternative
	t.Cleanup(func() { capture = oldCapture })

	body := `{"tool_name":"Edit","tool_input":{"file_path":"queue.go","new_string":"new KafkaClient()"}}`
	req := httptest.NewRequest(http.MethodPost, "http://x/api/guard", strings.NewReader(body))
	w := httptest.NewRecorder()

	httpGuard(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Result().StatusCode)
	}
	var out struct {
		Decided bool          `json:"decided"`
		Closed  []source.Atom `json:"closed"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Decided {
		t.Fatal("decided = false; want true for a tool-call reaching for ruled-out Kafka")
	}
	if len(out.Closed) == 0 {
		t.Fatal("want the matched closed decision, got none")
	}
	got := out.Closed[0]
	// Assert on the JSON contract the terminal actually renders (Text / ClosedNote /
	// Ref) — MatchKey and Score are internal matcher fields, not on the wire.
	if !strings.Contains(got.Text+" "+got.ClosedNote, "Kafka") {
		t.Fatalf("want the ruled-out option (Kafka) named in the rendered fields, got %+v", got)
	}
	if got.ClosedNote == "" {
		t.Fatal("want the recorded why-not (ClosedNote) the terminal renders")
	}
	if got.Ref == "" {
		t.Fatal("want a citation (Ref) on the matched closed decision")
	}
}

// A tool-call that reaches for nothing ruled out is SILENT: decided=false, no
// interception. This is the honesty invariant the whole product rests on —
// silence is not approval. The terminal renders nothing here, never a green check.
func TestHTTPGuardSilentWhenNothingRuledOut(t *testing.T) {
	oldCapture := capture
	capture = newTestStore(t) // closes Kafka / JWT — nothing about Redis
	t.Cleanup(func() { capture = oldCapture })

	body := `{"tool_name":"Edit","tool_input":{"file_path":"cache.go","new_string":"connect to Redis"}}`
	req := httptest.NewRequest(http.MethodPost, "http://x/api/guard", strings.NewReader(body))
	w := httptest.NewRecorder()

	httpGuard(w, req)

	var out struct {
		Decided bool          `json:"decided"`
		Closed  []source.Atom `json:"closed"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Decided || len(out.Closed) != 0 {
		t.Fatalf("a benign edit must be silent (decided=false, no interception), got %+v", out)
	}
}

// LEMA_GUARD_MODE=off is the kill switch: it must silence /api/guard too, even on
// a real hit — disabling the guard can't leave one surface still intercepting.
func TestHTTPGuardHonorsOffKillSwitch(t *testing.T) {
	oldCapture := capture
	capture = newTestStore(t) // closes Kafka
	t.Cleanup(func() { capture = oldCapture })
	t.Setenv(guardModeEnvVar, guardModeOff)

	body := `{"tool_name":"Edit","tool_input":{"file_path":"q.go","new_string":"new KafkaClient()"}}`
	req := httptest.NewRequest(http.MethodPost, "http://x/api/guard", strings.NewReader(body))
	w := httptest.NewRecorder()
	httpGuard(w, req)

	var out struct {
		Decided bool `json:"decided"`
	}
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Decided {
		t.Fatal("LEMA_GUARD_MODE=off must silence /api/guard even on a Kafka hit")
	}
}
