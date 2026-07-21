package main

// settle_test pins lema settle v1's contract (pivot B1, A2 77c99992):
// every verb drafts through POST /decisions/{id}/events and the output
// carries the browser-bind deep link — the command must never claim to have
// bound anything. These are intent tests: if settle ever stops printing the
// bind link, or starts treating a terminal draft as a binding act, the
// feature's honesty contract (draft-here, bind-in-browser) is broken and
// these fail.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const (
	// The two ids share the 6-char prefix "77c999" on purpose: unique
	// resolution needs 8 chars, and "77c999" exercises the ambiguity path.
	settleTestID  = "77c99992-1111-2222-3333-444455556666"
	settleOtherID = "77c99988-aaaa-bbbb-cccc-ddddeeeeffff"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// it printed. settle's user-facing contract IS its stdout.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}
	return string(out)
}

// newSettleTestServer stubs the three hosted endpoints settle touches and
// records appended events by decision id.
func newSettleTestServer(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var appended []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("GET /decisions/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/decisions/"), "/")
		switch id {
		case settleTestID:
			json.NewEncoder(w).Encode(map[string]any{
				"id": settleTestID, "title": "B1 internal order", "current_status": "accepted"})
		case settleOtherID:
			json.NewEncoder(w).Encode(map[string]any{
				"id": settleOtherID, "title": "a proposed draft", "current_status": "proposed"})
		default:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"error": "decision not found"})
		}
	})
	mux.HandleFunc("POST /decisions/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/decisions/"), "/events")
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		req["_decision_id"] = id
		// The server's real contract: rejected is proposed→rejected only.
		if req["type"] == "rejected" && id == settleTestID {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{"error": "decision cannot be rejected from its current status (only proposed → rejected is allowed)"})
			return
		}
		appended = append(appended, req)
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{"id": id, "current_status": "accepted"})
	})
	mux.HandleFunc("GET /workspaces/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"decisions": []map[string]any{
			{"id": settleTestID, "title": "B1 internal order", "current_status": "accepted"},
			{"id": settleOtherID, "title": "a proposed draft", "current_status": "proposed"},
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &appended
}

func settleEnv(t *testing.T, apiURL string) {
	t.Helper()
	t.Setenv("LEMA_API_URL", apiURL)
	t.Setenv("LEMA_API_TOKEN", "test-token")
	t.Setenv(workspaceIDEnv, "ws-test")
	t.Setenv("HOME", t.TempDir()) // never read the operator's real credentials file
}

func TestSettleAcceptDraftsAndPrintsBindLink(t *testing.T) {
	srv, appended := newSettleTestServer(t)
	settleEnv(t, srv.URL)
	t.Setenv(settleWebURLEnv, "https://web.example")

	out := captureStdout(t, func() {
		if err := runSettle([]string{"accept", settleTestID}); err != nil {
			t.Fatalf("runSettle accept: %v", err)
		}
	})

	if len(*appended) != 1 {
		t.Fatalf("expected 1 appended event, got %d", len(*appended))
	}
	ev := (*appended)[0]
	if ev["type"] != "accepted" {
		t.Fatalf("expected accepted event, got %v", ev["type"])
	}
	// The honesty contract: the draft is labeled a draft, and the bind deep
	// link — the one action that makes it a ruling — is in the output.
	wantLink := "https://web.example/decisions/" + settleTestID
	if !strings.Contains(out, wantLink) {
		t.Fatalf("output missing bind deep link %q:\n%s", wantLink, out)
	}
	if !strings.Contains(out, "DRAFT") {
		t.Fatalf("output must say the adjudication is a draft:\n%s", out)
	}
	if strings.Contains(strings.ToLower(out), "bound") {
		t.Fatalf("output must never claim the ruling is bound:\n%s", out)
	}
}

func TestSettleAcceptResolvesHandoffStylePrefix(t *testing.T) {
	srv, appended := newSettleTestServer(t)
	settleEnv(t, srv.URL)

	// "77c99992" is exactly how ids appear in HANDOFF notes; "d_77c99992" is
	// the search_decisions locator form. Both must resolve.
	for _, form := range []string{"77c99992", "d_77c99992", "lema:d_77c99992"} {
		*appended = nil
		out := captureStdout(t, func() {
			if err := runSettle([]string{"accept", form}); err != nil {
				t.Fatalf("runSettle accept %q: %v", form, err)
			}
		})
		if len(*appended) != 1 || (*appended)[0]["_decision_id"] != settleTestID {
			t.Fatalf("prefix %q did not resolve to %s", form, settleTestID)
		}
		if !strings.Contains(out, settleTestID) {
			t.Fatalf("output for %q missing resolved id", form)
		}
	}
}

func TestSettleAcceptAmbiguousPrefixFailsHonestly(t *testing.T) {
	srv, appended := newSettleTestServer(t)
	settleEnv(t, srv.URL)

	// "77c999" matches BOTH fixture decisions — must error naming the
	// candidates, before any write; "77c" is under the 6-char floor — must
	// error as malformed. A valid id in the same invocation still lands.
	err := runSettle([]string{"accept", "77c99992", "77c999", "77c"})
	if err == nil || !strings.Contains(err.Error(), "2 of 3 accepts failed") {
		t.Fatalf("expected 2-of-3 failure summary, got %v", err)
	}
	if len(*appended) != 1 {
		t.Fatalf("expected exactly 1 write (valid id only), got %d", len(*appended))
	}
}

func TestSettleRejectRequiresReasonAndSurfacesServerConflict(t *testing.T) {
	srv, appended := newSettleTestServer(t)
	settleEnv(t, srv.URL)

	if err := runSettle([]string{"reject", settleOtherID}); err == nil || !strings.Contains(err.Error(), "--reason") {
		t.Fatalf("reject without --reason must fail naming the flag, got %v", err)
	}
	if len(*appended) != 0 {
		t.Fatal("a validation failure must not write")
	}

	// Rejecting a non-proposed decision: the server's 409 must surface with
	// the server's own explanation, not a bare status code.
	err := runSettle([]string{"reject", settleTestID, "--reason", "dup"})
	if err == nil || !strings.Contains(err.Error(), "only proposed") {
		t.Fatalf("expected server conflict message to surface, got %v", err)
	}

	if err := runSettle([]string{"reject", settleOtherID, "--reason", "superseded by the amended plan"}); err != nil {
		t.Fatalf("reject proposed: %v", err)
	}
	ev := (*appended)[len(*appended)-1]
	payload := ev["payload"].(map[string]any)
	if payload["reason_body"] != "superseded by the amended plan" || payload["reason_category"] != "declined" {
		t.Fatalf("reject payload wrong: %v", payload)
	}
}

func TestSettleSupersedeLinksSuccessor(t *testing.T) {
	srv, appended := newSettleTestServer(t)
	settleEnv(t, srv.URL)

	out := captureStdout(t, func() {
		if err := runSettle([]string{"supersede", settleTestID, "--by", settleOtherID, "--reason", "amended"}); err != nil {
			t.Fatalf("runSettle supersede: %v", err)
		}
	})
	ev := (*appended)[len(*appended)-1]
	if ev["type"] != "superseded" {
		t.Fatalf("expected superseded event, got %v", ev["type"])
	}
	payload := ev["payload"].(map[string]any)
	if payload["superseded_by_id"] != settleOtherID {
		t.Fatalf("superseded_by_id wrong: %v", payload)
	}
	if !strings.Contains(out, "superseded by: "+settleOtherID) {
		t.Fatalf("output missing successor line:\n%s", out)
	}
}

func TestSettleNoCredentialsFailsBeforeAnyNetwork(t *testing.T) {
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")
	t.Setenv("HOME", t.TempDir())
	err := runSettle([]string{"accept", settleTestID})
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("expected credentials error, got %v", err)
	}
}

func TestSettleUsageOnUnknownVerb(t *testing.T) {
	srv, _ := newSettleTestServer(t)
	settleEnv(t, srv.URL)
	err := runSettle([]string{"bind", settleTestID})
	if err == nil || !strings.Contains(err.Error(), "unknown settle verb") {
		t.Fatalf("expected unknown-verb usage error, got %v", err)
	}
	if fmt.Sprint(err) == "" || !strings.Contains(err.Error(), "Confirm ruling") {
		t.Fatalf("usage must explain the browser bind, got %v", err)
	}
}
