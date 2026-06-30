package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// The pending store bridges the PreToolUse hook (which posts a tool-call and polls
// for the result) and the terminal UI (which lists open interceptions and resolves
// them): an added interception is OPEN until a human resolves it, after which it
// carries the resolution and drops out of the open list.
func TestGuardPendingStore(t *testing.T) {
	s := newGuardPendingStore()
	id := s.add("Edit", []source.Atom{{Ref: "d_1", Text: "Kafka — ops burden", ClosedNote: "ruled out"}})
	if id == "" {
		t.Fatal("add must return a non-empty id")
	}

	open := s.open()
	if len(open) != 1 || open[0].ID != id || open[0].Tool != "Edit" {
		t.Fatalf("open must list the added pending, got %+v", open)
	}
	if len(open[0].Closed) != 1 || open[0].Closed[0].Ref != "d_1" {
		t.Fatalf("pending must carry the interception atoms, got %+v", open[0].Closed)
	}

	if !s.resolve(id, "respect", "") {
		t.Fatal("resolve must succeed for a known id")
	}
	if len(s.open()) != 0 {
		t.Fatal("a resolved pending must drop from the open list")
	}
	got, ok := s.get(id)
	if !ok || got.Resolution != "respect" {
		t.Fatalf("get must show the resolution, got %+v ok=%v", got, ok)
	}

	if s.resolve("nope", "respect", "") {
		t.Fatal("resolve must return false for an unknown id")
	}
}

// POST /api/guard on a hit creates an open pending and returns its id; the terminal
// then discovers it via GET /api/guard/pending with the cited interception to render.
func TestHTTPGuardCreatesPendingOnHit(t *testing.T) {
	oldCapture := capture
	capture = newTestStore(t)
	guardPendings = newGuardPendingStore()
	t.Cleanup(func() { capture = oldCapture; guardPendings = newGuardPendingStore() })

	body := `{"tool_name":"Edit","tool_input":{"file_path":"q.go","new_string":"new KafkaClient()"}}`
	w := httptest.NewRecorder()
	httpGuard(w, httptest.NewRequest(http.MethodPost, "http://x/api/guard", strings.NewReader(body)))
	var posted struct {
		Decided bool   `json:"decided"`
		ID      string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&posted); err != nil {
		t.Fatalf("decode post: %v", err)
	}
	if !posted.Decided || posted.ID == "" {
		t.Fatalf("a hit must create a pending and return its id, got %+v", posted)
	}

	pw := httptest.NewRecorder()
	httpGuardPending(pw, httptest.NewRequest(http.MethodGet, "http://x/api/guard/pending", nil))
	var listed struct {
		Pending []guardPending `json:"pending"`
	}
	if err := json.NewDecoder(pw.Body).Decode(&listed); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	if len(listed.Pending) != 1 || listed.Pending[0].ID != posted.ID {
		t.Fatalf("the open pending must be listed for the terminal, got %+v", listed.Pending)
	}
	if len(listed.Pending[0].Closed) == 0 || !strings.Contains(listed.Pending[0].Closed[0].Text, "Kafka") {
		t.Fatalf("the listed pending must carry the cited interception, got %+v", listed.Pending)
	}
}

// The poll/callback resolution flow: the hook polls GET /api/guard/result (not
// resolved yet), the human POSTs /api/guard/resolve, then the hook's poll sees the
// resolution and the pending drops out of the open list.
func TestHTTPGuardResolveFlow(t *testing.T) {
	guardPendings = newGuardPendingStore()
	t.Cleanup(func() { guardPendings = newGuardPendingStore() })
	id := guardPendings.add("Edit", []source.Atom{{Ref: "d_1", Text: "Kafka"}})

	rw := httptest.NewRecorder()
	httpGuardResult(rw, httptest.NewRequest(http.MethodGet, "http://x/api/guard/result?id="+id, nil))
	var res struct {
		Resolved   bool   `json:"resolved"`
		Resolution string `json:"resolution"`
	}
	if err := json.NewDecoder(rw.Body).Decode(&res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.Resolved {
		t.Fatal("result must report not-resolved before the human acts")
	}

	body := `{"id":"` + id + `","resolution":"respect"}`
	resw := httptest.NewRecorder()
	httpGuardResolve(resw, httptest.NewRequest(http.MethodPost, "http://x/api/guard/resolve", strings.NewReader(body)))
	if resw.Result().StatusCode != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200", resw.Result().StatusCode)
	}

	rw2 := httptest.NewRecorder()
	httpGuardResult(rw2, httptest.NewRequest(http.MethodGet, "http://x/api/guard/result?id="+id, nil))
	if err := json.NewDecoder(rw2.Body).Decode(&res); err != nil {
		t.Fatalf("decode result2: %v", err)
	}
	if !res.Resolved || res.Resolution != "respect" {
		t.Fatalf("result must report the human's resolution, got %+v", res)
	}

	pw := httptest.NewRecorder()
	httpGuardPending(pw, httptest.NewRequest(http.MethodGet, "http://x/api/guard/pending", nil))
	var listed struct {
		Pending []guardPending `json:"pending"`
	}
	if err := json.NewDecoder(pw.Body).Decode(&listed); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	if len(listed.Pending) != 0 {
		t.Fatalf("a resolved pending must not stay open, got %+v", listed.Pending)
	}
}
