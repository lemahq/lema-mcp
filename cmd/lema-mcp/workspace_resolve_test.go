package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func resetWorkspaceUUIDCache(t *testing.T) {
	t.Helper()
	oldCache := workspaceUUIDCache
	workspaceUUIDCache = newWorkspaceUUIDCache(64, workspaceUUIDCacheTTL, time.Now)
	t.Cleanup(func() { workspaceUUIDCache = oldCache })
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
	resetWorkspaceUUIDCache(t)

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

func TestWorkspaceTargetResolverRejectsInvalidRepositoryRows(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer lema_live_x" {
			t.Fatalf("authorization = %q, want bearer credential", got)
		}
		switch r.URL.Path {
		case "/workspaces":
			_, _ = w.Write([]byte(`{"workspaces":[{"id":"repo-invalid","org_id":"org-1","is_repo":true},{"id":"project-payments","org_id":"org-1","is_repo":false}]}`))
		case "/workspaces/project-payments/links":
			_, _ = w.Write([]byte(`{"links":[{"workspace_id":"repo-invalid"}]}`))
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer ts.Close()

	resolver := newHostedTargetResolver(ts.Client(), ts.URL, "lema_live_x")
	input := resolveTargetInput{APIURL: ts.URL, CredentialFingerprint: credentialFingerprint("lema_live_x"), OrganizationID: "org-1"}
	for _, tc := range []struct {
		name string
		in   resolveTargetInput
	}{
		{"explicit workspace", resolveTargetInput{ExplicitWorkspaceID: "repo-invalid"}},
		{"explicit project", resolveTargetInput{ExplicitProjectID: "project-payments"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.APIURL = input.APIURL
			tc.in.CredentialFingerprint = input.CredentialFingerprint
			tc.in.OrganizationID = input.OrganizationID
			result, err := resolver.Resolve(context.Background(), tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status == resolutionResolved || result.Context.Repository.Canonical != "" {
				t.Fatalf("invalid repository row resolved a context: %#v", result)
			}
		})
	}
}

func TestWorkspaceValueUUIDCacheExpiresMutableSlugAndStaysBounded(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	oldCache := workspaceUUIDCache
	workspaceUUIDCache = newWorkspaceUUIDCache(2, time.Minute, func() time.Time { return now })
	t.Cleanup(func() { workspaceUUIDCache = oldCache })

	listings := map[string]string{
		"repo":  "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"repo2": "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"repo3": "cccccccc-cccc-cccc-cccc-cccccccccccc",
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entries := make([]map[string]string, 0, len(listings))
		for slug, id := range listings {
			entries = append(entries, map[string]string{"id": id, "slug": slug})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workspaces": entries})
	}))
	defer ts.Close()

	if got, err := resolveWorkspaceValueUUID(context.Background(), ts.Client(), ts.URL, "token", "repo"); err != nil || got != listings["repo"] {
		t.Fatalf("initial slug resolution = (%q, %v), want %q", got, err, listings["repo"])
	}
	listings["repo"] = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	now = now.Add(time.Minute + time.Nanosecond)
	if got, err := resolveWorkspaceValueUUID(context.Background(), ts.Client(), ts.URL, "token", "repo"); err != nil || got != listings["repo"] {
		t.Fatalf("expired slug resolution = (%q, %v), want current target %q", got, err, listings["repo"])
	}
	for _, slug := range []string{"repo2", "repo3"} {
		if _, err := resolveWorkspaceValueUUID(context.Background(), ts.Client(), ts.URL, "token", slug); err != nil {
			t.Fatalf("resolve %q: %v", slug, err)
		}
	}
	if got := workspaceUUIDCache.len(); got > 2 {
		t.Fatalf("cache size = %d, want capacity 2", got)
	}
}
