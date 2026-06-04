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

// TestCaptureReflectsExternalWrites reproduces the cross-process staleness bug:
// the long-lived `serve --http` store (the GUI's backend) must see a decision a
// SEPARATE process (a stdio lema-mcp the agent calls) wrote to the same
// decisions.jsonl — without a restart — or never-reopen silently regresses.
func TestCaptureReflectsExternalWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lema", "decisions.jsonl")

	// The long-lived reader loads first, while the file does not yet exist.
	server, err := NewCaptureStore(path)
	if err != nil {
		t.Fatal(err)
	}

	// A separate process records a killed option against the same file.
	writer, err := NewCaptureStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Record(DecisionRecord{
		Title:    "Primary datastore",
		Chosen:   "Postgres",
		Rejected: []RejectedAlt{{Option: "MongoDB", Why: "we need multi-row transactions"}},
	}); err != nil {
		t.Fatal(err)
	}

	// The long-lived reader must now enforce never-reopen without being recreated.
	if got := server.CheckDecided("MongoDB", 10); len(got) == 0 {
		t.Fatal("long-lived store did not see another process's CLOSED decision (stale in-memory cache)")
	}
}

// TestCaptureReflectsExternalSupersede covers the append-after-load path: the
// reader has already loaded a live choice, then another process supersedes it.
// The reader must return the old choice CLOSED on its next read.
func TestCaptureReflectsExternalSupersede(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.jsonl")

	// Seed a live decision and load the long-lived reader from it.
	seed, _ := NewCaptureStore(path)
	old, _ := seed.Record(DecisionRecord{Title: "API transport", Chosen: "REST"})

	server, err := NewCaptureStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if live := server.CheckDecided("REST transport", 10); len(live) != 0 {
		t.Fatalf("REST is the live choice; nothing should be CLOSED yet, got %+v", live)
	}

	// A separate process supersedes it.
	writer, _ := NewCaptureStore(path)
	if _, err := writer.Record(DecisionRecord{
		Title: "API transport v2", Chosen: "gRPC", Supersedes: []string{old.ID},
	}); err != nil {
		t.Fatal(err)
	}

	// Without a restart, the long-lived reader must now flag REST CLOSED.
	found := false
	for _, a := range server.CheckDecided("REST transport", 10) {
		if a.Type == "chosen" && a.Closed && strings.Contains(a.ClosedNote, "superseded") {
			found = true
		}
	}
	if !found {
		t.Fatal("long-lived store did not see another process's superseding write (stale cache)")
	}
}

// TestClosedAtomsReturnsAllClosed covers the no-query enforcement feed: every
// currently-CLOSED atom (rejected alternatives + superseded choices) and nothing
// live. This is what the cockpit's enforcement rail lists.
func TestClosedAtomsReturnsAllClosed(t *testing.T) {
	s, _ := NewCaptureStore(filepath.Join(t.TempDir(), "d.jsonl"))
	// A rejected alternative (closed) alongside a live chosen (open).
	s.Record(DecisionRecord{
		Title: "Primary datastore", Chosen: "Postgres",
		Rejected: []RejectedAlt{{Option: "MongoDB", Why: "need multi-row transactions"}},
	})
	// A superseded choice (closed): record REST, then supersede it with gRPC.
	old, _ := s.Record(DecisionRecord{Title: "API transport", Chosen: "REST"})
	s.Record(DecisionRecord{Title: "API transport v2", Chosen: "gRPC", Supersedes: []string{old.ID}})

	var sawMongo, sawREST bool
	for _, a := range s.ClosedAtoms() {
		if !a.Closed {
			t.Errorf("ClosedAtoms returned a non-closed atom: %+v", a)
		}
		if a.Type == "rejected_alternative" && strings.Contains(a.Text, "MongoDB") {
			sawMongo = true
		}
		if a.Type == "chosen" && strings.Contains(a.Text, "REST") {
			sawREST = true
		}
	}
	if !sawMongo {
		t.Error("expected the rejected MongoDB alternative in ClosedAtoms")
	}
	if !sawREST {
		t.Error("expected the superseded REST choice in ClosedAtoms")
	}
}

func TestClosedAtomsNilSafe(t *testing.T) {
	var s *CaptureStore
	if got := s.ClosedAtoms(); got != nil {
		t.Errorf("nil store ClosedAtoms = %v, want nil", got)
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
