package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The terminal-mode hook (guard_terminal.go) and the terminal's resolver UI both
// reach the /api/guard* COORDINATION routes WITHOUT an auth token: the hook is
// unattended engine code that carries none (guardViaTerminal uses a bare client),
// and v1's security boundary is the 127.0.0.1 bind, not a token (design lock —
// "localhost bind is the boundary for v1"). So withToken must let the four
// /api/guard* routes through like /healthz, while every OTHER /api/ route — in
// particular the persisted write /api/record — stays token-guarded. Without this
// exemption the hook's POST /api/guard 401s, fails its JSON decode, and silently
// fails open: a bare terminal that never intercepts.
func TestWithTokenExemptsGuardCoordinationRoutes(t *testing.T) {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	for _, p := range []string{
		"/api/guard", "/api/guard/pending", "/api/guard/resolve", "/api/guard/result",
		"/api/record", "/api/decided",
	} {
		mux.HandleFunc(p, ok)
	}
	handler := withToken("secret", mux)

	// Without a token: the guard coordination routes must pass (the tokenless hook
	// depends on this); every other /api/ route must still be rejected.
	noToken := []struct {
		path string
		want int
	}{
		{"/api/guard", http.StatusOK},
		{"/api/guard/pending", http.StatusOK},
		{"/api/guard/resolve", http.StatusOK},
		{"/api/guard/result", http.StatusOK},
		{"/api/record", http.StatusUnauthorized},
		{"/api/decided", http.StatusUnauthorized},
	}
	for _, tc := range noToken {
		req := httptest.NewRequest(http.MethodGet, "http://x"+tc.path, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if got := w.Result().StatusCode; got != tc.want {
			t.Errorf("%s without token = %d, want %d", tc.path, got, tc.want)
		}
	}

	// The exemption must not be path-prefix sloppy: a route that merely starts with
	// the guard string but is NOT a guard coordination route must stay guarded.
	mux.HandleFunc("/api/guardian", ok)
	req := httptest.NewRequest(http.MethodGet, "http://x/api/guardian", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if got := w.Result().StatusCode; got != http.StatusUnauthorized {
		t.Errorf("/api/guardian without token = %d, want 401 (not a guard coordination route)", got)
	}

	// And a token-guarded route still works WHEN the token is presented (the
	// terminal carries it for the override write); no regression.
	req = httptest.NewRequest(http.MethodGet, "http://x/api/record", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if got := w.Result().StatusCode; got != http.StatusOK {
		t.Errorf("/api/record WITH token = %d, want 200", got)
	}
}
