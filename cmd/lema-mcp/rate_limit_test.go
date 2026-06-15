package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// TestPublicAskRateLimitedConvertsGracefully pins MCP.5: a 429 becomes an honest
// "free limit reached → connect your repo / account" convert, NOT a hard error
// and NOT ransom on the answer (the cap is hit; nothing was withheld).
func TestPublicAskRateLimitedConvertsGracefully(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	_, out, err := publicAsk(context.Background(), nil, publicAskInput{Repo: "react", Query: "q"})
	if err != nil {
		t.Fatalf("rate-limit must convert, not error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.Answer), "limit") {
		t.Errorf("answer should explain the free limit: %q", out.Answer)
	}
	if out.Upgrade == "" || !strings.Contains(out.Upgrade, "utm_source=lema-mcp") {
		t.Errorf("rate-limit should carry an attributed upgrade CTA: %q", out.Upgrade)
	}
	if len(out.Sources) != 0 {
		t.Errorf("rate-limit returns no sources: %+v", out.Sources)
	}
	// Honesty: never ransom-on-the-answer framing.
	hay := strings.ToLower(out.Upgrade + " " + out.Answer)
	for _, bad := range []string{"pay to unlock", "unlock this", "withheld", "purchase this"} {
		if strings.Contains(hay, bad) {
			t.Errorf("must not read as ransom on the answer (found %q)", bad)
		}
	}
}
