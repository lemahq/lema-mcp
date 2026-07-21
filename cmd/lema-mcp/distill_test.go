package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The orchestration is fail-open and gate-first: it scans + posts only when the
// distiller is on (gate), credentials resolved, a transcript + session id exist,
// and this isn't a re-entrant stop — and any scan/post failure degrades to a no-op
// (never wedges the session, never blocks the stop). Mirrors TestPushRunner.
func TestDistillRunner(t *testing.T) {
	d := distilled{Text: "User: use redis\n\nAssistant: chose redis", Repo: "lema"}
	base := stopHookInput{SessionID: "sess-1", TranscriptPath: "/x/session.jsonl"}
	okPost := func(context.Context, string, distilled) (int, error) { return 2, nil }
	gateOn := func(context.Context) bool { return true }

	t.Run("distiller disabled (gate false) -> no scan, no post (nothing read or sent)", func(t *testing.T) {
		scanned, posted := false, false
		r := distillRunner{
			gate:    func(context.Context) bool { return false },
			scan:    func(string) (distilled, error) { scanned = true; return d, nil },
			post:    func(context.Context, string, distilled) (int, error) { posted = true; return 0, nil },
			canPush: true,
		}
		if n := r.run(context.Background(), base); n != 0 || scanned || posted {
			t.Errorf("n=%d scanned=%v posted=%v, want 0/false/false — a disabled distiller must not read the transcript or transmit", n, scanned, posted)
		}
	})

	t.Run("no gate wired -> fail-closed no-op (no scan)", func(t *testing.T) {
		scanned := false
		r := distillRunner{
			scan: func(string) (distilled, error) { scanned = true; return d, nil },
			post: okPost, canPush: true, // gate nil
		}
		if n := r.run(context.Background(), base); n != 0 || scanned {
			t.Errorf("n=%d scanned=%v, want 0/false — a nil gate must fail closed", n, scanned)
		}
	})

	t.Run("no credentials -> no scan, no post", func(t *testing.T) {
		scanned, posted := false, false
		r := distillRunner{
			gate:    gateOn,
			scan:    func(string) (distilled, error) { scanned = true; return d, nil },
			post:    func(context.Context, string, distilled) (int, error) { posted = true; return 0, nil },
			canPush: false,
		}
		if n := r.run(context.Background(), base); n != 0 || scanned || posted {
			t.Errorf("n=%d scanned=%v posted=%v, want 0/false/false", n, scanned, posted)
		}
	})

	t.Run("stop_hook_active -> no-op (loop guard), gate never even asked", func(t *testing.T) {
		scanned := false
		in := base
		in.StopHookActive = true
		r := distillRunner{gate: gateOn, scan: func(string) (distilled, error) { scanned = true; return d, nil }, post: okPost, canPush: true}
		if n := r.run(context.Background(), in); n != 0 || scanned {
			t.Errorf("n=%d scanned=%v, want 0/false", n, scanned)
		}
	})

	t.Run("empty transcript path -> no-op", func(t *testing.T) {
		in := base
		in.TranscriptPath = ""
		r := distillRunner{gate: gateOn, scan: func(string) (distilled, error) { return d, nil }, post: okPost, canPush: true}
		if n := r.run(context.Background(), in); n != 0 {
			t.Errorf("n=%d, want 0", n)
		}
	})

	t.Run("empty session id -> no-op (no read)", func(t *testing.T) {
		scanned := false
		in := base
		in.SessionID = ""
		r := distillRunner{gate: gateOn, scan: func(string) (distilled, error) { scanned = true; return d, nil }, post: okPost, canPush: true}
		if n := r.run(context.Background(), in); n != 0 || scanned {
			t.Errorf("n=%d scanned=%v, want 0/false — no session id means nothing to key the source by", n, scanned)
		}
	})

	t.Run("happy path -> posts the scrubbed deliberation, returns claims", func(t *testing.T) {
		var gotSession string
		var gotD distilled
		r := distillRunner{
			gate: gateOn,
			scan: func(string) (distilled, error) { return d, nil },
			post: func(_ context.Context, sessionID string, got distilled) (int, error) {
				gotSession, gotD = sessionID, got
				return 3, nil
			},
			canPush: true,
		}
		if n := r.run(context.Background(), base); n != 3 {
			t.Fatalf("n=%d, want 3 (the server's claim count)", n)
		}
		if gotSession != "sess-1" || gotD.Text != d.Text {
			t.Errorf("posted session=%q text=%q, want sess-1 + the scanned deliberation", gotSession, gotD.Text)
		}
	})

	t.Run("no prose to harvest (empty scan) -> no post", func(t *testing.T) {
		posted := false
		r := distillRunner{
			gate:    gateOn,
			scan:    func(string) (distilled, error) { return distilled{}, nil },
			post:    func(context.Context, string, distilled) (int, error) { posted = true; return 0, nil },
			canPush: true,
		}
		if n := r.run(context.Background(), base); n != 0 || posted {
			t.Errorf("n=%d posted=%v, want 0/false — no deliberation means nothing to send", n, posted)
		}
	})

	t.Run("scan error -> fail-open no-op", func(t *testing.T) {
		r := distillRunner{gate: gateOn, scan: func(string) (distilled, error) { return distilled{}, errors.New("boom") }, post: okPost, canPush: true}
		if n := r.run(context.Background(), base); n != 0 {
			t.Errorf("n=%d, want 0 on scan error", n)
		}
	})

	t.Run("post error -> fail-open no-op", func(t *testing.T) {
		r := distillRunner{
			gate:    gateOn,
			scan:    func(string) (distilled, error) { return d, nil },
			post:    func(context.Context, string, distilled) (int, error) { return 0, errors.New("boom") },
			canPush: true,
		}
		if n := r.run(context.Background(), base); n != 0 {
			t.Errorf("n=%d, want 0 on post error (incl. the dark 404 when the flag is off server-side)", n)
		}
	})
}

// The privacy scrub is load-bearing: a pasted secret in a prose turn is redacted
// before it can cross the wire, while the reasoning itself survives. Only PROSE
// turns are assembled — a tool_use-only assistant turn and a tool_result-only user
// turn (mechanical chatter) are excluded. The repo label is derived best-effort.
func TestDistillScanScrubsSecret(t *testing.T) {
	lines := []string{
		`{"type":"user","cwd":"/Users/x/myrepo","message":{"content":"Let's auth with the token sk-verysecretkey12345 and move on"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"I chose Redis for the session store because it is simplest here."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"file_path":"store.go","new_string":"redis.NewClient()"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"user","message":{"content":"<command-name>/clear</command-name>"}}`,
	}

	got := distillDeliberation(strings.NewReader(strings.Join(lines, "\n")))

	if strings.Contains(got.Text, "sk-verysecretkey12345") {
		t.Errorf("the pasted secret leaked into the payload:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "[redacted]") {
		t.Errorf("want the secret replaced with [redacted], got:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "Redis for the session store") {
		t.Errorf("the assistant's reasoning must survive scrubbing, got:\n%s", got.Text)
	}
	if !strings.Contains(got.Text, "auth with the token") {
		t.Errorf("the user's prose (minus the secret) must survive, got:\n%s", got.Text)
	}
	// Mechanical chatter is excluded — tool_use input text and tool_result bodies.
	if strings.Contains(got.Text, "redis.NewClient()") || strings.Contains(got.Text, "store.go") {
		t.Errorf("a tool_use-only turn must not be assembled into the deliberation, got:\n%s", got.Text)
	}
	// System-injected envelopes (angle-bracket) are not deliberation.
	if strings.Contains(got.Text, "command-name") {
		t.Errorf("a system envelope turn must be skipped, got:\n%s", got.Text)
	}
	if got.Repo != "myrepo" {
		t.Errorf("repo = %q, want myrepo (basename of the shortest cwd)", got.Repo)
	}
}

// The distiller gate is the hosted env-wide WorkOS flag lema-session-distill,
// fetched from the API (the Stop hook has no WorkOS session). The client asks GET
// /session-distill-enabled (Bearer auth) and fails CLOSED — it reads/transmits the
// transcript only when the API affirmatively returns enabled:true. Mirrors
// TestPushProducerEnabled.
func TestDistillEnabledGate(t *testing.T) {
	t.Run("enabled true -> on; sends GET /session-distill-enabled with bearer auth", func(t *testing.T) {
		var gotMethod, gotPath, gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": true})
		}))
		defer srv.Close()
		if !sessionDistillEnabled(context.Background(), srv.Client(), srv.URL, "lema_live_abc") {
			t.Error("want enabled (true)")
		}
		if gotMethod != http.MethodGet || gotPath != "/session-distill-enabled" {
			t.Errorf("request = %s %s, want GET /session-distill-enabled", gotMethod, gotPath)
		}
		if gotAuth != "Bearer lema_live_abc" {
			t.Errorf("auth = %q, want Bearer lema_live_abc", gotAuth)
		}
	})

	t.Run("enabled false -> off", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]bool{"enabled": false})
		}))
		defer srv.Close()
		if sessionDistillEnabled(context.Background(), srv.Client(), srv.URL, "t") {
			t.Error("want disabled (false)")
		}
	})

	t.Run("server error -> fail closed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		if sessionDistillEnabled(context.Background(), srv.Client(), srv.URL, "t") {
			t.Error("5xx must fail closed (false)")
		}
	})

	t.Run("unreachable API -> fail closed", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		if sessionDistillEnabled(context.Background(), &http.Client{}, url, "t") {
			t.Error("transport error must fail closed (false)")
		}
	})
}

// The ingest-session client speaks the server's contract exactly: POST to the
// workspace ingest-session path, Bearer auth, the {session_id, transcript, repo}
// body, and it reads back the server's claim count.
func TestDistillPostRequestShapeAndAuth(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotReq distillRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_ = json.NewEncoder(w).Encode(distillResponse{SessionID: gotReq.SessionID, Status: "ingested", Claims: 4})
	}))
	defer srv.Close()

	d := distilled{Text: "User: use redis\n\nAssistant: chose redis", Repo: "lema"}
	n, err := postSessionDistill(context.Background(), srv.Client(), srv.URL, "lema_live_abc", "ws_123", "sess-9", d)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n != 4 {
		t.Errorf("claims = %d, want 4 (server-reported)", n)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/workspaces/ws_123/ingest-session" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer lema_live_abc" {
		t.Errorf("auth = %q, want Bearer lema_live_abc", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q", gotCT)
	}
	if gotReq.SessionID != "sess-9" || gotReq.Transcript != d.Text || gotReq.Repo != "lema" {
		t.Errorf("body = %+v, want the session id + scrubbed transcript + repo", gotReq)
	}
}

// A non-2xx from the server (including the dark 404 when the flag is off
// server-side) is an error the hook swallows (fail-open), but the client must
// surface it rather than claim a silent success.
func TestDistillPostNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := postSessionDistill(context.Background(), srv.Client(), srv.URL, "t", "ws", "s", distilled{Text: "User: hi"})
	if err == nil {
		t.Fatal("want an error on HTTP 404 (the dark endpoint), got nil")
	}
}
