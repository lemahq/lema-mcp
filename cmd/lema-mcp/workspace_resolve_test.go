package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// #348: hosted record_decision used to hard-fail when LEMA_WORKSPACE_ID was
// unset, with no discoverable path to the id — a sibling session lost 4
// captures to it mid-flight. The whole point of capture is that it costs
// nothing at the moment a decision lands ("the lazy path must be the correct
// path"), so a resolvable config gap must never eat a record:
//   - exactly one visible workspace  -> auto-resolve, push there, and SAY SO in
//     the tool response (the human can pin it later);
//   - several                        -> a self-serve error naming each
//     workspace (slug + id) and the env var to set — the agent fixes it
//     without a human URL-hunt;
//   - none / fetch failed            -> an actionable error, never a bare
//     "env var unset".
// These tests pin that contract end to end through recorder.record.

type fakeWorkspacesAPI struct {
	t          *testing.T
	workspaces []map[string]any
	listStatus int32 // non-200 to fail the listing; settable per test
	listHits   int32
	importHits int32
	importedTo atomic.Value // last workspace id an import landed on
}

func (f *fakeWorkspacesAPI) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/workspaces":
			atomic.AddInt32(&f.listHits, 1)
			if got := r.Header.Get("Authorization"); got != "Bearer lema_live_x" {
				f.t.Errorf("GET /workspaces auth = %q, want the same bearer token the push uses", got)
			}
			if st := atomic.LoadInt32(&f.listStatus); st != 0 && st != http.StatusOK {
				w.WriteHeader(int(st))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": f.workspaces})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/import-decisions"):
			atomic.AddInt32(&f.importHits, 1)
			ws := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/workspaces/"), "/import-decisions")
			f.importedTo.Store(ws)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"created": 1,
				"results": []map[string]any{{"status": "created", "current_status": "accepted", "decision_id": "dec_42"}},
			})
		default:
			f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func autoResolvingRecorderFor(ts *httptest.Server) recorder {
	return recorder{pushHosted: newWorkspaceAutoResolvingPush(ts.URL, "lema_live_x", ts.Client())}
}

func TestRecordAutoResolvesSingleWorkspace(t *testing.T) {
	f := &fakeWorkspacesAPI{t: t, workspaces: []map[string]any{
		{"id": "ws-archived", "slug": "old-repo", "name": "old repo", "archived_at": "2026-01-01T00:00:00Z"},
		{"id": "ws-live", "slug": "lemahq-lema", "name": "lemahq/lema"},
	}}
	ts := f.server()
	defer ts.Close()

	out, err := autoResolvingRecorderFor(ts).record(context.Background(), sampleDecisionRecord())
	if err != nil {
		t.Fatalf("record: %v — a single visible workspace must auto-resolve, not fail", err)
	}
	if got := f.importedTo.Load(); got != "ws-live" {
		t.Errorf("import landed on workspace %v, want the one ACTIVE workspace ws-live (archived must not count)", got)
	}
	low := strings.ToLower(out.Recorded)
	if !strings.Contains(low, "auto-resolved") || !strings.Contains(out.Recorded, "lemahq-lema") {
		t.Errorf("tool response %q must say the workspace was auto-resolved and name it", out.Recorded)
	}
	if !strings.Contains(out.Recorded, workspaceIDEnv) {
		t.Errorf("tool response %q must name %s so the human can pin the choice", out.Recorded, workspaceIDEnv)
	}
}

func TestRecordAutoResolveAmbiguousListsWorkspaces(t *testing.T) {
	f := &fakeWorkspacesAPI{t: t, workspaces: []map[string]any{
		{"id": "ws-a", "slug": "repo-a", "name": "repo a"},
		{"id": "ws-b", "slug": "repo-b", "name": "repo b"},
	}}
	ts := f.server()
	defer ts.Close()

	_, err := autoResolvingRecorderFor(ts).record(context.Background(), sampleDecisionRecord())
	if err == nil {
		t.Fatal("record succeeded with an ambiguous workspace set — it must not guess where a capture lands")
	}
	for _, want := range []string{"repo-a", "ws-a", "repo-b", "ws-b", workspaceIDEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q — the agent needs the ids and the env var to self-serve the fix (#348)", err, want)
		}
	}
	if n := atomic.LoadInt32(&f.importHits); n != 0 {
		t.Errorf("ambiguous resolution still pushed %d import(s)", n)
	}
}

func TestRecordAutoResolveNoWorkspaces(t *testing.T) {
	f := &fakeWorkspacesAPI{t: t, workspaces: []map[string]any{}}
	ts := f.server()
	defer ts.Close()

	_, err := autoResolvingRecorderFor(ts).record(context.Background(), sampleDecisionRecord())
	if err == nil {
		t.Fatal("record succeeded with no visible workspaces")
	}
	if !strings.Contains(err.Error(), workspaceIDEnv) || !strings.Contains(strings.ToLower(err.Error()), "workspace") {
		t.Errorf("error %q must explain there is no visible workspace and name %s", err, workspaceIDEnv)
	}
}

// A transient listing failure must not be memoized: the next record_decision
// retries the resolution, so one blip does not poison the whole session — and
// a SUCCESSFUL resolution is memoized, so steady state costs one extra GET
// total, not one per capture.
func TestRecordAutoResolveRetriesAfterFetchFailureThenMemoizes(t *testing.T) {
	// The id is a real UUID (as the server always returns) so the downstream
	// pushDecisions slug→UUID resolve short-circuits and adds no GET — the two
	// hits counted below are the auto-resolver's own failed+memoized attempts.
	f := &fakeWorkspacesAPI{t: t, workspaces: []map[string]any{
		{"id": "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", "slug": "lemahq-lema", "name": "lemahq/lema"},
	}}
	f.listStatus = http.StatusInternalServerError
	ts := f.server()
	defer ts.Close()
	rec := autoResolvingRecorderFor(ts)

	_, err := rec.record(context.Background(), sampleDecisionRecord())
	if err == nil {
		t.Fatal("record succeeded while the workspace listing was failing")
	}
	if !strings.Contains(err.Error(), workspaceIDEnv) {
		t.Errorf("fetch-failure error %q must still name %s as the manual path", err, workspaceIDEnv)
	}

	atomic.StoreInt32(&f.listStatus, http.StatusOK)
	for i := 0; i < 2; i++ {
		if _, err := rec.record(context.Background(), sampleDecisionRecord()); err != nil {
			t.Fatalf("record after listing recovered: %v", err)
		}
	}
	if n := atomic.LoadInt32(&f.listHits); n != 3 {
		t.Errorf("GET /workspaces hit %d times, want 3 (one failed attempt + auto-resolution + validated UUID push)", n)
	}
	if n := atomic.LoadInt32(&f.importHits); n != 2 {
		t.Errorf("imports = %d, want 2", n)
	}
}

// The recorder wiring guard: validation still precedes any network call, so a
// malformed capture never burns the resolution round-trip.
func TestRecordAutoResolveValidatesFirst(t *testing.T) {
	f := &fakeWorkspacesAPI{t: t}
	ts := f.server()
	defer ts.Close()

	_, err := autoResolvingRecorderFor(ts).record(context.Background(), source.DecisionRecord{Title: "  "})
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("err = %v, want the title/chosen validation error", err)
	}
	if n := atomic.LoadInt32(&f.listHits); n != 0 {
		t.Errorf("validation failure still listed workspaces %d time(s)", n)
	}
}

func TestWorkspaceListingDecodesAuthoritativeTargetFields(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/workspaces" {
			t.Fatalf("request = %s %s, want GET /workspaces", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer lema_live_x" {
			t.Fatalf("authorization = %q, want bearer credential", got)
		}
		_, _ = w.Write([]byte(`{"workspaces":[{"id":"repo-api","org_id":"org-1","repo_url":"ssh://git@github.acme.internal:2222/acme/api.git","is_repo":true}]}`))
	}))
	defer ts.Close()

	entries, err := fetchWorkspaces(context.Background(), ts.Client(), ts.URL, "lema_live_x")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].OrgID != "org-1" || entries[0].RepoURL == "" || !entries[0].IsRepo {
		t.Fatalf("entries = %#v, want decoded org/repo/is_repo fields", entries)
	}
	workspaces := targetWorkspacesFromEntries(entries)
	if len(workspaces) != 1 || workspaces[0].OrganizationID != "org-1" || !workspaces[0].IsRepository || workspaces[0].Repository.Canonical != "git:github.acme.internal:2222/acme/api" {
		t.Fatalf("target workspaces = %#v, want authoritative repository mapping", workspaces)
	}
}

func TestWorkspaceLinksUseAuthenticatedContainerPath(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/workspaces/project-payments/links" {
			t.Fatalf("request = %s %s, want GET /workspaces/project-payments/links", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer lema_live_x" {
			t.Fatalf("authorization = %q, want bearer credential", got)
		}
		_, _ = w.Write([]byte(`{"links":[{"workspace_id":"repo-api"},{"workspace_id":"repo-web"}]}`))
	}))
	defer ts.Close()

	links, err := fetchWorkspaceLinks(context.Background(), ts.Client(), ts.URL, "lema_live_x", "project-payments")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(links, ","), "repo-api,repo-web"; got != want {
		t.Fatalf("links = %q, want %q", got, want)
	}
}

func TestWorkspaceTargetResolverAdapterUsesVisibleContainersAndLinks(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer lema_live_x" {
			t.Fatalf("authorization = %q, want bearer credential", got)
		}
		switch r.URL.Path {
		case "/workspaces":
			_, _ = w.Write([]byte(`{"workspaces":[{"id":"repo-api","org_id":"org-1","repo_url":"https://github.com/acme/api.git","is_repo":true},{"id":"project-payments","org_id":"org-1","is_repo":false}]}`))
		case "/workspaces/project-payments/links":
			_, _ = w.Write([]byte(`{"links":[{"workspace_id":"repo-api"}]}`))
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer ts.Close()

	resolver := newHostedTargetResolver(ts.Client(), ts.URL, "lema_live_x")
	input := resolveTargetInput{APIURL: ts.URL, CredentialFingerprint: credentialFingerprint("lema_live_x"), OrganizationID: "org-1"}
	workspaces, err := resolver.workspaces(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	links, err := resolver.fetchLinks(context.Background(), input, "project-payments")
	if err != nil {
		t.Fatal(err)
	}
	if len(workspaces) != 2 || workspaces[0].Repository.Canonical != "git:github.com/acme/api" || strings.Join(links, ",") != "repo-api" {
		t.Fatalf("resolver adapters returned workspaces=%#v links=%#v", workspaces, links)
	}
}

func TestWorkspaceValueUUIDValidatesUUIDAndIsolatesCredentialCache(t *testing.T) {
	workspaceUUIDMu.Lock()
	workspaceUUIDCache = map[string]string{}
	workspaceUUIDMu.Unlock()

	const visibleUUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	var requests int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		switch r.Header.Get("Authorization") {
		case "Bearer token-a":
			_, _ = w.Write([]byte(`{"workspaces":[{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"}]}`))
		case "Bearer token-b":
			_, _ = w.Write([]byte(`{"workspaces":[{"id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}]}`))
		default:
			t.Fatalf("unexpected authorization %q", r.Header.Get("Authorization"))
		}
	}))
	defer ts.Close()

	if got, err := resolveWorkspaceValueUUID(context.Background(), ts.Client(), ts.URL, "token-a", visibleUUID); err != nil || got != visibleUUID {
		t.Fatalf("token-a resolution = (%q, %v), want visible UUID", got, err)
	}
	if _, err := resolveWorkspaceValueUUID(context.Background(), ts.Client(), ts.URL, "token-b", visibleUUID); err == nil {
		t.Fatal("UUID accepted from token-a cache for token-b despite token-b visibility")
	}
	if _, err := resolveWorkspaceValueUUID(context.Background(), ts.Client(), ts.URL, "token-a", "cccccccc-cccc-cccc-cccc-cccccccccccc"); err == nil {
		t.Fatal("invisible UUID passed through without validating GET /workspaces")
	}
	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Fatalf("GET /workspaces calls = %d, want 3 (token isolation plus UUID rejection)", got)
	}
}
