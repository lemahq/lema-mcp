package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWithTokenDoesNotAllowQueryToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/test", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := withToken("secret", mux)

	// Query token should be rejected for security
	req := httptest.NewRequest(http.MethodGet, "http://x/api/test?token=secret", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Result().StatusCode; got != http.StatusUnauthorized {
		t.Errorf("Query parameter token should be rejected for security, got status %d", got)
	}

	// Bearer token should be accepted
	req = httptest.NewRequest(http.MethodGet, "http://x/api/test", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Result().StatusCode; got != http.StatusOK {
		t.Errorf("Bearer token should be accepted, got status %d", got)
	}
}
