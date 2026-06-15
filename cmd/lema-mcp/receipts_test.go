package main

import (
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

func f64(v float64) *float64 { return &v }

func TestSourceReceipt(t *testing.T) {
	got := sourceReceipt(source.AskSource{
		Status:               "superseded",
		RejectedAlternatives: []string{"mixins"},
		Relevance:            f64(0.81),
	})
	if !strings.Contains(got, "superseded — do not cite as current") {
		t.Errorf("missing superseded flag: %q", got)
	}
	if !strings.Contains(got, "ruled out: mixins (do not re-propose)") {
		t.Errorf("missing ruled-out list: %q", got)
	}
	if !strings.Contains(got, "relevance 0.81 (cosine)") {
		t.Errorf("relevance must be labeled cosine: %q", got)
	}
	if strings.Contains(strings.ToLower(got), "confidence") {
		t.Errorf("relevance must NEVER be labeled confidence: %q", got)
	}
}

func TestSourceReceiptInForceIsClean(t *testing.T) {
	if got := sourceReceipt(source.AskSource{Status: "accepted"}); strings.Contains(got, "do not cite") {
		t.Errorf("in-force decision must not carry a superseded flag: %q", got)
	}
}

func TestRoiNote(t *testing.T) {
	if got := roiNote(source.AskUsage{}, true); got != "" {
		t.Errorf("abstain roi note = %q, want empty", got)
	}
	got := roiNote(source.AskUsage{AtomsTokens: 180, SourceTokens: 3400, CompressionRatio: 18.9}, false)
	if !strings.Contains(got, "180 atom-tokens vs 3400 source-body tokens") {
		t.Errorf("grounded roi note wrong: %q", got)
	}
}
