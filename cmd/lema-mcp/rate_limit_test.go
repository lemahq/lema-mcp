package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// TestCheckApproachRateLimitedConvertsGracefully pins MCP.5 on the surviving public
// door (ADR-0124): a 429 becomes an honest "free limit reached → connect your repo /
// account" convert, NOT a hard error and NOT ransom on the answer (the cap is hit;
// nothing was withheld). Moved here from the dropped why_decided handler, which
// shared the same rateLimitedUpgradeCTA const.
func TestCheckApproachRateLimitedConvertsGracefully(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	out, err := runCheckApproach(context.Background(), "check_approach", "react", "q")
	if err != nil {
		t.Fatalf("rate-limit must convert, not error: %v", err)
	}
	if !strings.Contains(strings.ToLower(out.Note), "limit") {
		t.Errorf("note should explain the free limit: %q", out.Note)
	}
	if out.Upgrade == "" || !strings.Contains(out.Upgrade, "utm_source=lema-mcp") {
		t.Errorf("rate-limit should carry an attributed upgrade CTA: %q", out.Upgrade)
	}
	if len(out.Sources) != 0 {
		t.Errorf("rate-limit returns no sources: %+v", out.Sources)
	}
	// Honesty: never ransom-on-the-answer framing.
	hay := strings.ToLower(out.Upgrade + " " + out.Note)
	for _, bad := range []string{"pay to unlock", "unlock this", "withheld", "purchase this"} {
		if strings.Contains(hay, bad) {
			t.Errorf("must not read as ransom on the answer (found %q)", bad)
		}
	}
}
