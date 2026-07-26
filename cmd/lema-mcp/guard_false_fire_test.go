package main

import (
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// Regression pins for the never-reopen guard's false fires of 2026-07-25/26.
// Each asserts WHY the match is wrong — a crossed subject domain, or prose that
// merely MENTIONS a rejected option — not merely the current output.

func falseFireStore(t *testing.T) []source.Atom {
	t.Helper()
	return storeWith(t,
		// d_e45c79's shape: the chosen name is "The Record", the rejected option is
		// the bare "Record" — a one-word key that is an ordinary English word.
		source.DecisionRecord{Title: "name the knowledge destination", Chosen: "The Record",
			Rejected: []source.RejectedAlt{{Option: "Record", Why: "ambiguous when spoken"}}},
		// ADR-0019's shape: a killed option built entirely from ordinary words.
		source.DecisionRecord{Title: "how accepted decisions reach the repo", Chosen: "GitHub round-trip via a private App",
			Rejected: []source.RejectedAlt{{Option: "Direct commit to `main`", Why: "bypasses CODEOWNERS and review"}}},
		kafkaQueueRecord,
	).ClosedAtoms()
}

// MISFIRE 3: a design doc naming the record-conflicts FEATURE and the flag
// record_conflicts_enabled. Domain boundary: a product-NAMING decision about the
// durable knowledge destination vs an unrelated feature name plus an ordinary verb.
// Writing prose that contains the word "record" is not proposing to call the
// knowledge destination "Record".
func TestGuardNoFire_RecordInFeatureName(t *testing.T) {
	closed := falseFireStore(t)
	doc := `# Record-conflicts panel

The /conflicts page ships behind ` + "`record_conflicts_enabled`" + `.
It shows where two recorded decisions disagree, reading recorded decisions
from the graph and rendering the conflicting pair side by side.`

	if out, atom := evaluateGuard(closed, ctxQuery("docs/design/conflicts.md", doc), "docs/design/conflicts.md", guardModeContext); out != nil {
		t.Fatalf("a feature named \"record-conflicts\" and the verb \"recorded\" must not "+
			"re-propose the rejected product name \"Record\": fired %+v", atom)
	}
}

// MISFIRE 2: a CI deploy-topology document. Domain boundary: ADR-0019 governs the
// GIT WORKFLOW for materialising accepted decisions (PR vs direct push); this doc
// changes DEPLOY topology and the work shipped entirely through PRs. The words
// "direct", "commit" and "main" co-occurring in deploy prose are not a proposal to
// bypass review.
func TestGuardNoFire_DeployTopologyProse(t *testing.T) {
	closed := falseFireStore(t)
	doc := `# Ship direct to production

We ship main directly to production; stage becomes a cold standby.
Each commit builds once and deploys straight to prod behind the smoke gate.
The release job cuts one version tag per commit.
Every change still lands as a pull request.`

	if out, atom := evaluateGuard(closed, ctxQuery("docs/design/ship.md", doc), "docs/design/ship.md", guardModeContext); out != nil {
		t.Fatalf("deploy-topology prose must not re-propose ADR-0019's \"Direct commit to `main`\": fired %+v", atom)
	}
}

// The recall side of the same rule: the guard is not gutted for prose. A document
// that genuinely NAMES a killed option — capitalised the way the option is — still
// fires, and so does a code edit reaching it through a lowercase identifier.
func TestGuardStillFires_RealReproposals(t *testing.T) {
	closed := falseFireStore(t)

	// Prose naming the product: capitalised, so it is the option, not a word.
	out, _ := evaluateGuard(closed, ctxQuery("plan.md", "now wire up the Kafka consumer here"), "plan.md", guardModeContext)
	if out == nil {
		t.Error("prose naming Kafka as the product must still fire")
	}
	// Code reaching it through a lowercase identifier — the shipped pin.
	out, _ = evaluateGuard(closed, ctxQuery("internal/queue/kafka.go", "kafka.NewProducer()"), "internal/queue/kafka.go", guardModeContext)
	if out == nil {
		t.Error("kafka.NewProducer() in code must still fire")
	}
	// A multi-word option NAMED contiguously in a doc still fires.
	out, _ = evaluateGuard(closed, ctxQuery("plan.md", "let's just do a Direct commit to `main` for this one"), "plan.md", guardModeContext)
	if out == nil {
		t.Error("a document naming the killed option contiguously must still fire")
	}
}

// containsCased is the prose discriminator; pin its edges directly.
func TestContainsCased(t *testing.T) {
	cases := []struct {
		text, key string
		want      bool
	}{
		{"now wire up the Kafka consumer", "Kafka", true},
		{"a kafka topic per tenant", "Kafka", false},        // lowercase prose usage
		{"the record-conflicts panel", "Record", false},     // lowercase
		{"record_conflicts_enabled", "Record", false},       // lowercase identifier
		{"auto-accept into the Record now", "Record", true}, // capitalised usage
		{"the Recorder service", "Record", false},           // whole-word only
		{"the Record-conflicts panel", "Record", false},     // hyphenated compound is its own term
		{"prefixRecord", "Record", false},                   // whole-word only
	}
	for _, c := range cases {
		if got := containsCased(c.text, c.key); got != c.want {
			t.Errorf("containsCased(%q, %q) = %v, want %v", c.text, c.key, got, c.want)
		}
	}
}
