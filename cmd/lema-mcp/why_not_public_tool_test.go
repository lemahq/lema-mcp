package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// why_not_public is now a deprecated thin alias for settled — both call runSettled
// and return a settledOutput. These tests verify the alias wires up correctly and
// that the /settled endpoint is called (no query template, no Authorization header).

func TestWhyNotPublicCallsSettledEndpoint(t *testing.T) {
	var gotPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("Authorization") != "" {
			t.Errorf("must send no Authorization header, got %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repo": "react-rfcs", "topic": "mixins",
			"settled": "settled",
			"decisions": []map[string]any{{
				"ref":    "reactjs/rfcs#68",
				"reason": "Mixins introduce implicit dependencies and name collisions; Hooks were chosen instead.",
			}},
			"note": "",
		})
	}))
	defer ts.Close()
	prev := publicSrc
	publicSrc = source.NewPublic(ts.URL, ts.Client())
	defer func() { publicSrc = prev }()

	_, out, err := whyNotPublic(context.Background(), nil, whyNotPublicInput{Repo: "react", Option: "mixins"})
	if err != nil {
		t.Fatalf("whyNotPublic: %v", err)
	}
	if !strings.HasSuffix(gotPath, "/settled") {
		t.Errorf("expected /settled endpoint, got path %q", gotPath)
	}
	if out.State != "settled" {
		t.Errorf("expected state=settled, got %q", out.State)
	}
	if len(out.Decisions) != 1 || out.Decisions[0].Ref != "reactjs/rfcs#68" {
		t.Errorf("expected one decision with ref reactjs/rfcs#68, got %+v", out.Decisions)
	}
	if !strings.Contains(out.Decisions[0].Reason, "Mixins") {
		t.Errorf("reason should mention Mixins, got %q", out.Decisions[0].Reason)
	}
}

func TestWhyNotPublicUnknownRepo(t *testing.T) {
	prev := publicSrc
	publicSrc = source.NewPublic("http://unused.invalid", nil)
	defer func() { publicSrc = prev }()
	if _, _, err := whyNotPublic(context.Background(), nil, whyNotPublicInput{Repo: "django", Option: "x"}); err == nil {
		t.Fatalf("expected error for unknown repo")
	}
}
