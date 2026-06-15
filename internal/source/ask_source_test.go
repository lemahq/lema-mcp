package source

import (
	"encoding/json"
	"testing"
)

// TestAskSourceDecodesTrustFields pins WP1: the api already serves
// rejected_alternatives + relevance on the /ask(-public) wire, but the MCP's
// AskSource struct lacked the fields, so json silently dropped them. They must
// now decode. (relevance is a *float64 so an fts-only atom with no value stays
// nil rather than a faked 0.0.)
func TestAskSourceDecodesTrustFields(t *testing.T) {
	const wire = `{
		"n": 1, "ref": "reactjs/rfcs#68", "type": "rejected",
		"text": "hand-rolled cache invalidation was ruled out",
		"status": "superseded",
		"rejected_alternatives": ["a global mutable cache", "manual invalidation"],
		"relevance": 0.81
	}`
	var s AskSource
	if err := json.Unmarshal([]byte(wire), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(s.RejectedAlternatives) != 2 || s.RejectedAlternatives[0] != "a global mutable cache" {
		t.Fatalf("rejected_alternatives = %+v, want 2 items", s.RejectedAlternatives)
	}
	if s.Relevance == nil || *s.Relevance != 0.81 {
		t.Fatalf("relevance = %v, want 0.81", s.Relevance)
	}
}
