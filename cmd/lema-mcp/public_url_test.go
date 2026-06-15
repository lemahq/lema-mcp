package main

import "testing"

// TestDefaultPublicAPIURLIsBaked guards the release: the published package must
// ship a baked public-api URL so `npx lema-mcp` and `npx lema-mcp try <repo>`
// reach the public demo with zero config. An accidental revert to "" would
// silently disable public_ask for every install.
func TestDefaultPublicAPIURLIsBaked(t *testing.T) {
	if defaultPublicAPIURL == "" {
		t.Fatal("defaultPublicAPIURL must be baked for release (got empty)")
	}
	t.Setenv("LEMA_PUBLIC_API_URL", "")
	if got := resolvePublicAPIURL(); got != defaultPublicAPIURL {
		t.Fatalf("resolvePublicAPIURL() with no env = %q, want the baked %q", got, defaultPublicAPIURL)
	}
}
