package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestWithCORS(t *testing.T) {
	os.Setenv("LEMA_HTTP_ORIGIN", "http://localhost:3000,http://example.com")
	defer os.Unsetenv("LEMA_HTTP_ORIGIN")

	handler := withCORS(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name           string
		method         string
		origin         string
		expectedStatus int
		expectedCORS   string
	}{
		{
			name:           "Valid Origin GET",
			method:         "GET",
			origin:         "http://localhost:3000",
			expectedStatus: http.StatusOK,
			expectedCORS:   "http://localhost:3000",
		},
		{
			name:           "Another Valid Origin GET",
			method:         "GET",
			origin:         "http://example.com",
			expectedStatus: http.StatusOK,
			expectedCORS:   "http://example.com",
		},
		{
			name:           "Invalid Origin GET",
			method:         "GET",
			origin:         "https://attacker.com",
			expectedStatus: http.StatusOK,
			expectedCORS:   "",
		},
		{
			name:           "Valid Origin OPTIONS",
			method:         "OPTIONS",
			origin:         "http://localhost:3000",
			expectedStatus: http.StatusNoContent,
			expectedCORS:   "http://localhost:3000",
		},
		{
			name:           "Invalid Origin OPTIONS",
			method:         "OPTIONS",
			origin:         "https://attacker.com",
			expectedStatus: http.StatusForbidden,
			expectedCORS:   "",
		},
		{
			name:           "No Origin GET",
			method:         "GET",
			origin:         "",
			expectedStatus: http.StatusOK,
			expectedCORS:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "http://example.com/foo", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Result().StatusCode != tc.expectedStatus {
				t.Errorf("expected status %d, got %d", tc.expectedStatus, w.Result().StatusCode)
			}

			corsHeader := w.Header().Get("Access-Control-Allow-Origin")
			if corsHeader != tc.expectedCORS {
				t.Errorf("expected CORS header %q, got %q", tc.expectedCORS, corsHeader)
			}
		})
	}
}
