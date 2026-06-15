package source

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPublicAskRateLimitedOn429 pins MCP.5: a 429 from /ask-public (the per-IP
// rate limit or the per-day demo ceiling) maps to ErrPublicRateLimited so the
// caller can convert it honestly instead of surfacing a raw "status 429".
func TestPublicAskRateLimitedOn429(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()
	p := NewPublic(ts.URL, ts.Client())
	if _, err := p.PublicAsk(context.Background(), "react-rfcs", "q"); !errors.Is(err, ErrPublicRateLimited) {
		t.Fatalf("err = %v, want ErrPublicRateLimited", err)
	}
}
