package source

import (
	"path/filepath"
	"strings"
	"testing"
)

// A hosted-mode capture whose push failed is preserved as a LOCAL DRAFT
// (status proposed): durable and searchable, but non-binding — its rejected
// alternatives must not enforce never-reopen, because no human and no server
// trust tier has accepted it (the "a local accepted write would bind a draft
// the team has not accepted" rationale in record_decision.go, github.com/lemahq/lema-mcp).
func TestCaptureRecordDraftIsProposedAndNonBinding(t *testing.T) {
	s, err := NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	rec, err := s.RecordDraft(DecisionRecord{
		Title:    "queue for the ingest pipeline",
		Chosen:   "Kafka",
		Rejected: []RejectedAlt{{Option: "RabbitMQ", Why: "no replay"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "proposed" {
		t.Fatalf("draft status = %q, want proposed", rec.Status)
	}

	// The draft is durable and searchable — the write is never lost.
	atoms := s.Search("RabbitMQ ingest queue", 8)
	if len(atoms) == 0 {
		t.Fatal("draft did not surface in search")
	}

	// But nothing about it binds: the rejected alternative must not enforce.
	for _, a := range atoms {
		if a.Closed {
			t.Fatalf("draft atom %s is CLOSED — an unaccepted draft must not enforce", a.ID)
		}
	}
	if closed := s.CheckDecided("RabbitMQ for the ingest pipeline", 10); len(closed) != 0 {
		t.Fatalf("CheckDecided returned %d closed atom(s) from a draft", len(closed))
	}

	// The chosen atom says it is a draft, so a reader never mistakes it for a
	// settled ruling.
	var chosenText string
	for _, a := range atoms {
		if a.Type == "chosen" {
			chosenText = a.Text
		}
	}
	if !strings.Contains(strings.ToLower(chosenText), "draft") {
		t.Fatalf("chosen atom does not mark the draft: %q", chosenText)
	}
}

// A draft's supersedes edges must not close the decisions they point at — a
// failed push falling back locally must never un-bind a settled record.
func TestCaptureDraftSupersedesDoesNotCloseTarget(t *testing.T) {
	s, err := NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	prior, err := s.Record(DecisionRecord{Title: "primary database", Chosen: "PostgreSQL"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordDraft(DecisionRecord{
		Title: "primary database", Chosen: "SQLite", Supersedes: []string{prior.ID},
	}); err != nil {
		t.Fatal(err)
	}
	for _, a := range s.ClosedAtoms() {
		if a.Ref == prior.ID {
			t.Fatalf("draft supersedes closed the settled decision: %+v", a)
		}
	}
}

// Re-recording the same decision (same content id) through the accepted path
// upgrades the draft in place: last write wins, and the rejected alternative
// starts enforcing.
func TestCaptureDraftUpgradedByLaterAcceptedRecord(t *testing.T) {
	s, err := NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	in := DecisionRecord{
		Title:    "queue for the ingest pipeline",
		Chosen:   "Kafka",
		Rejected: []RejectedAlt{{Option: "RabbitMQ", Why: "no replay"}},
	}
	draft, err := s.RecordDraft(in)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Record(in)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != draft.ID {
		t.Fatalf("re-record forked the id: draft %s vs accepted %s", draft.ID, rec.ID)
	}
	if closed := s.CheckDecided("RabbitMQ for the ingest pipeline", 10); len(closed) == 0 {
		t.Fatal("accepted re-record did not start enforcing the rejected alternative")
	}
}
