package main

import (
	"bytes"
	"strings"
	"testing"
)

// The demo must render from the REAL capture path (record → check_decided),
// so the CLOSED line is genuine enforcement output, not a hardcoded string.
func TestDemoRendersRealNeverReopen(t *testing.T) {
	var buf bytes.Buffer
	if err := demoTo(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"TanStack Query",    // the chosen option, from the recorded decision
		"SWR",               // the rejected alternative
		"CLOSED",            // the enforcement marker
		"do not propose",    // the directive, computed by CheckDecided
		"never-reopen",      // the framing
		"npx lema-mcp init", // the wire-it-in CTA
	} {
		if !strings.Contains(out, want) {
			t.Errorf("demo output missing %q\n---\n%s", want, out)
		}
	}
}
