package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// TestNamesOption pins the lexical gate one question at a time. The cases are
// grouped by WHICH gate decides them, so a future change that loosens one gate
// fails on the cases that gate owns rather than somewhere unrelated.
func TestNamesOption(t *testing.T) {
	cases := []struct {
		name string
		key  string
		text string
		want bool
		gate string
	}{
		// POSITION — the key as the trailing element of a longer identifier is
		// the head noun being qualified, not a name being proposed.
		{"camel suffix", "Record", "type PublicRecord struct{}", false, "position"},
		{"camel suffix, call site", "Record", "h.PublicRecord.GetDecision(ctx, id)", false, "position"},
		{"camel suffix, constructor", "Record", "func NewPublicRecord() *PublicRecord", false, "position"},
		{"snake suffix", "Record", "table public_record holds it", false, "position"},
		{"basename", "Record", "public_record.go", false, "position"},
		// The mirror image: the key MODIFIES the declared thing, so the brand is
		// being invoked. d_194bff protects this — it is the guard's best case.
		{"camel prefix", "Kafka", "c := NewKafkaClient()", true, "position"},
		{"camel prefix, bare", "Kafka", "KafkaBrokers = []string{}", true, "position"},

		// CASE — a capitalized key is a proper name; lowercase prose is the
		// ordinary word. Waived only for path/package separators.
		{"lowercase prose", "Record", "the public record read surface", false, "case"},
		{"lowercase prose 2", "Knowledge", "that is knowledge the team already paid for", false, "case"},
		{"lowercase adjective", "Linear", "the matcher does a linear scan over the set", false, "case"},
		{"snake_case verb name", "Record", "call record_decision when it lands", false, "case"},
		{"snake_case index name", "Knowledge", "one active via jobs_one_active_knowledge_audit", false, "case"},
		{"quoted snake const", "Knowledge", `const Kind = "knowledge_audit"`, false, "case"},
		// Waived: package, file and path forms lowercase a product name by
		// convention. These are pinned behaviour elsewhere in the suite.
		{"package qualifier", "Kafka", "kafka.NewProducer()", true, "case-waived"},
		{"file basename", "Kafka", "kafka.go", true, "case-waived"},
		{"hyphenated basename", "Kafka", "kafka-adr-0140.go", true, "case-waived"},
		{"import cue", "Kafka", "import kafka", true, "case-waived"},
		{"route segment", "Record", `href="/record"`, true, "case-waived"},

		// DETERMINER — "the Record" USES a noun phrase, "use Record" MENTIONS
		// the name. English proper names reject articles.
		{"article", "Record", "never auto-accept into the Record", false, "determiner"},
		{"demonstrative", "Record", "that Record is already written", false, "determiner"},
		{"possessive", "Knowledge", "our Knowledge of the corpus is partial", false, "determiner"},
		{"adoption verb", "Record", "let's use Record for this", true, "determiner"},
		{"declaration keyword", "Record", "type Record struct {", true, "determiner"},
		{"markdown heading", "Record", "## Record", true, "determiner"},
		{"naming prose", "Record", "call the destination Record instead", true, "determiner"},
	}
	for _, c := range cases {
		if got := namesOption(c.key, c.text); got != c.want {
			t.Errorf("[%s] namesOption(%q, %q) = %v, want %v", c.gate, c.key, c.text, got, c.want)
		}
	}
}

// TestNamesOptionOnlyGatesSingleTokenKeys is the blast-radius pin: a multi-word
// key keeps the contiguous-run adjacency of pain-point #27 and never reaches
// the naming gate, so this fix cannot perturb the 497 corpus atoms it does not
// govern.
func TestNamesOptionOnlyGatesSingleTokenKeys(t *testing.T) {
	q := newGuardText("we should adopt the lema Workspaces naming here")
	if ok, _ := optionMatches("lema Workspaces", q); !ok {
		t.Error("a multi-word key must still match by adjacency, determiner or not")
	}
	if _, single := singleTokenKey("lema Workspaces"); single {
		t.Error("lema Workspaces must not be classified single-token")
	}
	if tok, single := singleTokenKey("Record"); !single || tok != "record" {
		t.Errorf("singleTokenKey(Record) = %q,%v; want record,true", tok, single)
	}
}

// TestGuardNovelHits pins the second gate: a one-word name already in the file
// is carried along by the edit, not proposed by it. This is what separates a
// NEW `/record` route from the `/record?repo=` already in public_record.go,
// which no lexical rule can do — they are character-identical.
func TestGuardNovelHits(t *testing.T) {
	hits := []source.Atom{
		{Ref: "d_e45c79", MatchKey: "Record"},
		{Ref: "d_e45c79", MatchKey: "Lema Relay"},
	}
	got := guardNovelHits(hits, "// the repo record page (/gh/{org}/{repo})\ntype PublicRecord struct{}")
	if len(got) != 1 || got[0].MatchKey != "Lema Relay" {
		t.Errorf("a name already in the file must be dropped; kept %v", keysOf(got))
	}
	// Multi-word keys are out of scope — novelty must not silence them.
	if got := guardNovelHits(hits, "we already discussed the Lema Relay option at length"); len(got) != 2 {
		t.Errorf("novelty is scoped to single-token keys; kept %v", keysOf(got))
	}
	// No baseline (a Write creating a file) leaves everything intact.
	if got := guardNovelHits(hits, ""); len(got) != 2 {
		t.Errorf("an absent baseline must fail open; kept %v", keysOf(got))
	}
}

func keysOf(atoms []source.Atom) []string {
	out := make([]string, 0, len(atoms))
	for _, a := range atoms {
		out = append(out, a.MatchKey)
	}
	return out
}

// TestGuardBaseline pins that the baseline is the file as it stands BEFORE the
// edit — PreToolUse runs before the write lands, so a plain read is correct and
// no git call is needed — and that every failure path yields "" (fail open).
func TestGuardBaseline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "public_record.go")
	if err := os.WriteFile(path, []byte("type PublicRecord struct{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := guardBaseline(map[string]any{"file_path": path}); got != "type PublicRecord struct{}" {
		t.Errorf("baseline = %q, want the pre-edit file content", got)
	}
	if got := guardBaseline(map[string]any{"file_path": filepath.Join(dir, "nope.go")}); got != "" {
		t.Errorf("a missing file (Write creating it) must yield no baseline, got %q", got)
	}
	if got := guardBaseline(map[string]any{}); got != "" {
		t.Errorf("no file_path must yield no baseline, got %q", got)
	}
}
