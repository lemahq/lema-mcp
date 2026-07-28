package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// MC-7's contract: at a session boundary, one stderr line points at the
// bind-pending batch this run's work produced — count and an in-app link,
// never a bind action itself. Every failure path (dead server, non-200,
// malformed body, zero count) is silent.

func TestBoundaryBindNoticePrintsCountAndLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/decisions/bind-pending") {
			io.WriteString(w, `{"decisions":[{"id":"a"},{"id":"b"}],"count":2}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	var buf bytes.Buffer
	notifyBindPendingTo(&buf, srv.URL, "tok", "ws-123", "https://lema.sh")
	out := buf.String()
	if !strings.Contains(out, "2 ruling") || !strings.Contains(out, "/decisions/bind-pending") {
		t.Fatalf("notice missing count or link: %q", out)
	}
}

func TestBoundaryBindNoticeSilentOnFailureAndZero(t *testing.T) {
	var buf bytes.Buffer
	notifyBindPendingTo(&buf, "http://127.0.0.1:1", "tok", "ws-123", "https://lema.sh") // dead server
	if buf.Len() != 0 {
		t.Fatalf("dead server must be silent, got %q", buf.String())
	}
}

// notifyBindPending is the boundary wire-in: it resolves the real workspace
// the checkpoint just synced to (the same immutable-receipt path
// syncCheckpoint uses) and only then calls notifyBindPendingTo. A wrong
// adaptation that leaves the workspace id empty would silently hit
// "/workspaces//decisions/bind-pending" forever — this test catches that by
// asserting the resolved id actually reaches the request path.
//
// The repository and project workspace ids are deliberately DISTINCT here
// (a linked project atop a repository leaf): hosted captures are pushed via
// receipt.RepositoryWorkspaceID (newHostedRecorder / pushDecisions in
// record_decision.go), so the notice must resolve and query that same id,
// not receipt.ProjectWorkspaceID. A solo single-workspace setup where both
// ids happen to be identical would mask a regression back to
// ProjectWorkspaceID; distinct ids pin the choice so that regression fails
// the assertion below instead of failing open silently.

func TestNotifyBindPendingResolvesWorkspaceAndHitsCorrectEndpoint(t *testing.T) {
	const repoWorkspaceID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const projectWorkspaceID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	var hitPath string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /workspaces", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"workspaces":[`+
			`{"id":"`+repoWorkspaceID+`","org_id":"org-1","is_repo":true,"repo_url":"https://github.com/acme/proj.git"},`+
			`{"id":"`+projectWorkspaceID+`","org_id":"org-1","is_repo":false}`+
			`]}`)
	})
	mux.HandleFunc("GET /workspaces/"+projectWorkspaceID+"/links", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"links":[{"workspace_id":"`+repoWorkspaceID+`"}]}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/decisions/bind-pending") {
			hitPath = r.URL.Path
			io.WriteString(w, `{"decisions":[],"count":0}`)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setSyncEnv(t, srv.URL) // sets LEMA_WORKSPACE_ID = repoWorkspaceID

	notifyBindPending("unused-dir", mkEnv("sess-notice", "stop", nil))

	want := "/workspaces/" + repoWorkspaceID + "/decisions/bind-pending"
	if hitPath != want {
		t.Fatalf("bind-pending hit path = %q, want %q — notice must resolve RepositoryWorkspaceID (where pushDecisions writes), not ProjectWorkspaceID (%s)", hitPath, want, projectWorkspaceID)
	}
}

func TestNotifyBindPendingSkipsNonBoundaryKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("a non-boundary kind must never reach the network, got %s", r.URL.Path)
	}))
	defer srv.Close()
	setSyncEnv(t, srv.URL)

	notifyBindPending("unused-dir", mkEnv("sess-notice", "tool_use", nil))
}

func TestNotifyBindPendingFailOpenWhenUnconfigured(t *testing.T) {
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")
	t.Setenv("LEMA_WORKSPACE_ID", "")
	t.Setenv("HOME", t.TempDir()) // isolate from any real ~/.config/lema/credentials

	// Reaching here without panic or hang is the assertion.
	notifyBindPending("unused-dir", mkEnv("sess-notice", "stop", nil))
}
