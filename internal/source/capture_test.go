package source

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureRecordAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lema", "decisions.jsonl")
	s, err := NewCaptureStore(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.Record(DecisionRecord{
		Title:    "State management for the web app",
		Chosen:   "TanStack Query",
		Rejected: []RejectedAlt{{Option: "SWR", Why: "no first-class mutations"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID == "" {
		t.Fatal("expected a generated id")
	}
	if rec.Status != "accepted" {
		t.Fatalf("new record status = %q, want accepted", rec.Status)
	}

	// A fresh store loaded from the same file sees the persisted record.
	s2, err := NewCaptureStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Len() != 1 {
		t.Fatalf("records after reload = %d, want 1", s2.Len())
	}
}

func TestCaptureRecordRequiresTitleAndChosen(t *testing.T) {
	s, _ := NewCaptureStore(filepath.Join(t.TempDir(), "d.jsonl"))
	if _, err := s.Record(DecisionRecord{Chosen: "x"}); err == nil {
		t.Error("expected error when title is empty")
	}
	if _, err := s.Record(DecisionRecord{Title: "x"}); err == nil {
		t.Error("expected error when chosen is empty")
	}
}

func TestCaptureRejectedIsClosed(t *testing.T) {
	s, _ := NewCaptureStore(filepath.Join(t.TempDir(), "d.jsonl"))
	if _, err := s.Record(DecisionRecord{
		Title:    "Caching layer",
		Chosen:   "TanStack Query",
		Rejected: []RejectedAlt{{Option: "SWR", Why: "no mutations"}},
	}); err != nil {
		t.Fatal(err)
	}
	var swr *Atom
	for _, a := range s.Search("should we use SWR for caching", 8) {
		if a.Type == "rejected_alternative" && strings.Contains(a.Text, "SWR") {
			a := a
			swr = &a
			break
		}
	}
	if swr == nil {
		t.Fatal("expected a rejected_alternative atom for SWR")
	}
	if !swr.Closed {
		t.Error("rejected alternative should be CLOSED")
	}
	if !strings.Contains(strings.ToLower(swr.ClosedNote), "do not propose") {
		t.Errorf("closed note missing directive: %q", swr.ClosedNote)
	}
}

func TestCaptureSupersedeClosesChosen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.jsonl")
	s, _ := NewCaptureStore(path)
	old, _ := s.Record(DecisionRecord{Title: "API transport", Chosen: "REST"})
	if _, err := s.Record(DecisionRecord{
		Title: "API transport v2", Chosen: "gRPC", Supersedes: []string{old.ID},
	}); err != nil {
		t.Fatal(err)
	}

	// Reload to exercise reduce()'s status derivation from the append log.
	s2, _ := NewCaptureStore(path)
	found := false
	for _, a := range s2.CheckDecided("REST transport", 10) {
		if a.Type == "chosen" && a.Closed && strings.Contains(a.ClosedNote, "superseded") {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the superseded REST choice to come back CLOSED")
	}
}

func TestCheckDecidedOnlyReturnsClosed(t *testing.T) {
	s, _ := NewCaptureStore(filepath.Join(t.TempDir(), "d.jsonl"))
	s.Record(DecisionRecord{
		Title:    "Primary datastore",
		Chosen:   "Postgres",
		Rejected: []RejectedAlt{{Option: "MongoDB", Why: "we need multi-row transactions"}},
	})

	got := s.CheckDecided("MongoDB", 10)
	if len(got) == 0 {
		t.Fatal("expected MongoDB to be CLOSED")
	}
	for _, a := range got {
		if !a.Closed {
			t.Errorf("CheckDecided returned a non-closed atom: %+v", a)
		}
	}
	// Postgres is the live choice (not superseded), so nothing about it is closed.
	if live := s.CheckDecided("Postgres", 10); len(live) != 0 {
		t.Errorf("the live choice should not be CLOSED, got %+v", live)
	}
}

func TestCaptureDedupByTitleAndChosen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.jsonl")
	s, _ := NewCaptureStore(path)
	s.Record(DecisionRecord{Title: "Linter", Chosen: "eslint", Rationale: "first pass"})
	s.Record(DecisionRecord{Title: "Linter", Chosen: "eslint", Rationale: "refined"})
	if s.Len() != 1 {
		t.Fatalf("records after re-recording same decision = %d, want 1", s.Len())
	}
	// And the reduction holds across a reload of the append log (which has 2 lines).
	s2, _ := NewCaptureStore(path)
	if s2.Len() != 1 {
		t.Fatalf("records after reload = %d, want 1", s2.Len())
	}
}
