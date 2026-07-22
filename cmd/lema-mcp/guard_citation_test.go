package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// citationTestStore records atoms shaped like the pain-point #4 specimens: options
// whose names are made of ordinary doc vocabulary, so citing their rejection in
// prose used to fire the guard.
func citationTestStore(t *testing.T) *source.CaptureStore {
	t.Helper()
	s, err := source.NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []source.DecisionRecord{
		{Title: "binding sweep UX", Chosen: "reviewed-list bulk confirm",
			Rejected: []source.RejectedAlt{{Option: "per-row-only confirms", Why: "ceremony not judgment"}}},
		{Title: "cross-repo transport", Chosen: "state brief",
			Rejected: []source.RejectedAlt{{Option: "relay", Why: "naming collision with the arc term"}}},
		{Title: "message queue", Chosen: "NATS",
			Rejected: []source.RejectedAlt{{Option: "Kafka", Why: "operational burden"}}},
	} {
		if _, err := s.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestStripCitationLines(t *testing.T) {
	cases := []struct {
		name, in, wantKept string
		wantStripped       bool
	}{
		{"no markers", "plain line one\nplain line two", "plain line one\nplain line two", false},
		{"rejected colon", "keep me\nsweep landed — rejected: per-row-only confirms\nkeep too",
			"keep me\nkeep too", true},
		{"rejected word case-insensitive", "This option was Rejected last week", "", true},
		{"rejected_alternative marker", "type=rejected_alternative per-row-only confirms", "", true},
		{"ruled out", "kafka was ruled out last year", "", true},
		{"ruled-out hyphen", "the ruled-out set includes kafka", "", true},
		{"supersedes", "this supersedes the relay naming", "", true},
		{"ADR ref", "we moved off relay per ADR-0140", "", true},
		{"ADR ref no hyphen", "see ADR0140 for relay", "", true},
		{"d_ decision ref", "kept the token matcher (d_045d82)", "", true},
		{"bare hex is NOT a marker", "landed in a2499f66 with kafka", "landed in a2499f66 with kafka", false},
		{"unrelated 'rejection' word forms do not over-match", "the rejection handler retries", "the rejection handler retries", false},
		{"killed: list idiom", "Killed: dedicated image/pipeline, Pub/Sub, relay", "", true},
		{"bare killed is NOT a marker", "the startup probe killed it", "the startup probe killed it", false},
	}
	for _, c := range cases {
		kept, stripped := stripCitationLines(c.in)
		if kept != c.wantKept || stripped != c.wantStripped {
			t.Errorf("%s: stripCitationLines(%q) = (%q, %v), want (%q, %v)",
				c.name, c.in, kept, stripped, c.wantKept, c.wantStripped)
		}
	}
}

func TestGuardCitationExemption(t *testing.T) {
	closed := citationTestStore(t).ClosedAtoms()

	// The pain #4 specimen: a HANDOFF line DOCUMENTING the rejected alternative
	// must not fire — writing down what was rejected is surfacing, not proposing.
	if out, _ := evaluateGuard(closed, ctxQuery("HANDOFF.md",
		"- ADR-0141 sweep landed — rejected: per-row-only confirms (ceremony not judgment)"), guardModeContext); out != nil {
		t.Fatalf("citing a rejection must not fire, got %+v", out)
	}
	// The "relay" specimen: naming the settled decision by its ADR ref is a cite.
	if out, _ := evaluateGuard(closed, ctxQuery("notes.md",
		"we moved off relay per ADR-0140"), guardModeContext); out != nil {
		t.Fatalf("ADR-ref cite must not fire, got %+v", out)
	}
	// "ruled out" prose is a cite.
	if out, _ := evaluateGuard(closed, ctxQuery("notes.md",
		"kafka was ruled out last year"), guardModeContext); out != nil {
		t.Fatalf("ruled-out cite must not fire, got %+v", out)
	}
	// Ask mode is exempt the same way (same matcher path).
	if out, _ := evaluateGuard(closed, ctxQuery("notes.md",
		"rejected: wire up Kafka consumer"), guardModeAsk); out != nil {
		t.Fatalf("ask-mode cite must not fire, got %+v", out)
	}

	// A plain re-proposal still fires.
	out, atom := evaluateGuard(closed, ctxQuery("plan.md",
		"let's implement per-row-only confirms for binding"), guardModeContext)
	if out == nil || atom == nil {
		t.Fatal("plain re-proposal must still fire")
	}
	// A mixed edit — a citation line PLUS a genuine proposal line — still fires:
	// the exemption is line-scoped, not whole-edit.
	out, _ = evaluateGuard(closed, ctxQuery("plan.md",
		"ADR-0141 landed the sweep\nnow wire up Kafka consumer here"), guardModeContext)
	if out == nil || !strings.Contains(out.HookSpecificOutput.AdditionalContext, "operational burden") {
		t.Fatalf("proposal on a non-citation line must fire, got %+v", out)
	}
	// Option pieces split across a citation line and a plain line must not fire:
	// citation text cannot supply match tokens.
	if out, _ := evaluateGuard(closed, ctxQuery("x.md",
		"per-row-only ceremony — rejected: yes\nthe confirms arrive later"), guardModeContext); out != nil {
		t.Fatalf("pieces split across a citation line must not fire, got %+v", out)
	}
	// The file basename is its own line in the query, so a citation first line
	// does not swallow it: an edit to kafka.go still fires on the basename.
	out, _ = evaluateGuard(closed, ctxQuery("kafka.go",
		"rejected: something unrelated\nplain code here"), guardModeContext)
	if out == nil {
		t.Fatal("basename must survive a citation first line and fire")
	}
	// INTENDED residual: the exemption is a whole-line bypass, so a genuine
	// adoption that shares a line with any marker — even an unrelated ref — is
	// exempt. Advisory mode + the citation-exempt log measure this trade.
	if out, _ := evaluateGuard(closed, ctxQuery("q.go",
		"use Kafka per ADR-0007 for retries"), guardModeContext); out != nil {
		t.Fatalf("marker-sharing line is exempt by design, got %+v", out)
	}
}

func TestCitationExemptAtom(t *testing.T) {
	closed := citationTestStore(t).ClosedAtoms()

	// A suppressed fire is reported for the calibration log.
	a := citationExemptAtom(closed, ctxQuery("HANDOFF.md",
		"sweep landed — rejected: per-row-only confirms (ceremony)"))
	if a == nil || a.MatchKey != "per-row-only confirms" {
		t.Fatalf("suppressed fire must surface the atom, got %+v", a)
	}
	// No citation lines → nothing suppressed.
	if a := citationExemptAtom(closed, ctxQuery("plan.md", "wire up Kafka consumer")); a != nil {
		t.Fatalf("no citation lines must report nil, got %+v", a)
	}
	// A real fire (kept lines still match) is not "suppressed".
	if a := citationExemptAtom(closed, ctxQuery("plan.md",
		"ADR-0141 landed\nwire up Kafka consumer")); a != nil {
		t.Fatalf("a surviving fire must report nil, got %+v", a)
	}
	// Citation lines with no underlying match → nil.
	if a := citationExemptAtom(closed, ctxQuery("notes.md",
		"rejected: an option nobody recorded")); a != nil {
		t.Fatalf("cite without a match must report nil, got %+v", a)
	}
}

func TestGuardLogCitationExempt(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "guard.log")
	t.Setenv("LEMA_GUARD_LOG", logPath)

	closed := citationTestStore(t).ClosedAtoms()
	in := guardInput{ToolName: "Edit"}
	query := ctxQuery("HANDOFF.md", "sweep landed — rejected: per-row-only confirms (ceremony)")
	a := citationExemptAtom(closed, query)
	if a == nil {
		t.Fatal("expected a suppressed atom")
	}
	guardLogWrite(in, "citation-exempt", query, a)

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("log line not JSON: %v: %s", err, b)
	}
	if rec["decision"] != "citation-exempt" {
		t.Errorf("decision = %v, want citation-exempt", rec["decision"])
	}
	if rec["match_key"] != "per-row-only confirms" {
		t.Errorf("match_key = %v, want the suppressed option", rec["match_key"])
	}
}

// The query joins parts on newlines so one part's citation marker cannot bleed
// into another part's text (and the basename stays its own line).
func TestGuardQueryNewlineJoin(t *testing.T) {
	q := guardQuery(map[string]any{
		"file_path":  "queue/kafka.go",
		"new_string": "rejected: old idea",
		"edits":      []any{map[string]any{"new_string": "second part"}},
	})
	lines := strings.Split(q, "\n")
	if len(lines) < 3 || lines[0] != "kafka.go" {
		t.Fatalf("expected basename on its own line, got %q", q)
	}
}
