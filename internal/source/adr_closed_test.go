package source

import (
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/adr"
)

func TestRejectedAlternatives(t *testing.T) {
	body := "## Decision\nWe chose NATS.\n\n" +
		"## Alternatives considered\n\n" +
		"### Kafka\n- **Summary:** a streaming broker\n- **Status:** rejected — operational burden for our scale\n\n" +
		"### NATS (chosen)\n- **Status:** chosen — simpler ops\n\n" +
		"## Consequences\n\n" +
		"### Some sub-point\n- not an alternative; must not be parsed as one\n"

	alts := rejectedAlternatives(body)
	if len(alts) != 1 {
		t.Fatalf("want 1 rejected alt (Kafka only), got %d: %+v", len(alts), alts)
	}
	if alts[0].name != "Kafka" {
		t.Fatalf("want name Kafka, got %q", alts[0].name)
	}
	if !strings.Contains(alts[0].reason, "operational burden") {
		t.Fatalf("want reason containing 'operational burden', got %q", alts[0].reason)
	}
}

func TestLocalClosedAtoms(t *testing.T) {
	three := 3
	adrs := []adr.ADR{
		{Number: 1, Title: "message queue", Status: "accepted",
			Body: "## Alternatives considered\n\n### Kafka\n- **Status:** rejected — ops burden\n"},
		{Number: 2, Title: "old state mgmt", Status: "accepted", SupersededBy: &three,
			Body: "## Alternatives considered\n\n### Redux\n- **Status:** rejected — boilerplate\n"},
		{Number: 4, Title: "draft", Status: "proposed",
			Body: "## Alternatives considered\n\n### Mongo\n- **Status:** rejected — nope\n"},
	}

	closed := NewLocal(adrs).ClosedAtoms()
	if len(closed) != 1 {
		t.Fatalf("want 1 closed atom (only the accepted, non-superseded ADR enforces), got %d: %+v", len(closed), closed)
	}
	c := closed[0]
	if c.MatchKey != "Kafka" || !c.Closed || c.Type != "rejected_alternative" {
		t.Fatalf("bad closed atom: %+v", c)
	}
	if !strings.Contains(c.ClosedNote, "Kafka") || !strings.Contains(c.ClosedNote, "message queue") {
		t.Fatalf("note should name the option + ADR title, got %q", c.ClosedNote)
	}
}
