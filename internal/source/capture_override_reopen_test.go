package source

import (
	"path/filepath"
	"testing"
)

// PROPOSAL (override re-flag semantics, lema-terminal Phase 2 follow-up). A human
// :override in the terminal records a NEW decision that supersedes the one whose
// ruling was overridden AND chooses the previously-rejected option. Today that
// reversal is recorded but does NOT stop the next agent being re-flagged: rejected-
// alternative atoms are closed UNCONDITIONALLY (buildAtoms), and supersession only
// closes the superseded decision's *chosen* atom. So the same edit re-fires forever.
//
// The proposed semantics (scoped, behind LEMA_OVERRIDE_REOPENS, default-off so the
// locked never-reopen invariant is unchanged until ratified): a rejected-alternative
// atom stops enforcing ONLY when a CURRENT (accepted, not itself superseded) decision
// SUPERSEDES the rejecting decision AND its chosen matches that option. This is the
// precedent-not-scripture case — an explicit, human-authored reversal "with the prior
// why in hand", not silent relitigation. The history stays in the record (bitemporal);
// the atom just no longer blocks.

// setOverrideReopens toggles the proposal flag for a test and returns a restore.
func setOverrideReopens(v bool) func() {
	prev := overrideReopens
	overrideReopens = v
	return func() { overrideReopens = prev }
}

func reopenStore(t *testing.T) *CaptureStore {
	t.Helper()
	s, err := NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func kafkaClosed(s *CaptureStore) bool {
	for _, a := range s.ClosedAtoms() {
		if a.Type == "rejected_alternative" && a.MatchKey == "Kafka" {
			return true
		}
	}
	return false
}

// With the flag ON: an override that supersedes the rejecting decision AND chooses
// the rejected option un-flags it. The next agent reaching for Kafka is not blocked.
func TestOverrideReopen_FlagOn_RechosenOptionUnflags(t *testing.T) {
	defer setOverrideReopens(true)()
	s := reopenStore(t)
	d1, _ := s.Record(DecisionRecord{
		Title: "message queue", Chosen: "NATS",
		Rejected: []RejectedAlt{{Option: "Kafka", Why: "operational burden"}},
	})
	if !kafkaClosed(s) {
		t.Fatal("precondition: Kafka should be closed before the override")
	}
	// The human override: chose Kafka, superseding D1.
	if _, err := s.Record(DecisionRecord{
		Title: "message queue (revisited)", Chosen: "Kafka",
		Rationale: "exactly-once delivery", Supersedes: []string{d1.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if kafkaClosed(s) {
		t.Fatal("after a superseding re-choice of Kafka, the rejected-Kafka atom must no longer enforce")
	}
}

// With the flag OFF (default): the locked invariant is unchanged — Kafka stays closed.
func TestOverrideReopen_FlagOff_InvariantUnchanged(t *testing.T) {
	defer setOverrideReopens(false)()
	s := reopenStore(t)
	d1, _ := s.Record(DecisionRecord{
		Title: "message queue", Chosen: "NATS",
		Rejected: []RejectedAlt{{Option: "Kafka", Why: "operational burden"}},
	})
	s.Record(DecisionRecord{
		Title: "message queue (revisited)", Chosen: "Kafka", Supersedes: []string{d1.ID},
	})
	if !kafkaClosed(s) {
		t.Fatal("with the flag off, the never-reopen default must hold: Kafka stays closed")
	}
}

// Scoped: superseding the decision but choosing something ELSE does NOT un-flag the
// rejected option (the team didn't adopt it).
func TestOverrideReopen_FlagOn_DifferentChoiceKeepsClosed(t *testing.T) {
	defer setOverrideReopens(true)()
	s := reopenStore(t)
	d1, _ := s.Record(DecisionRecord{
		Title: "message queue", Chosen: "NATS",
		Rejected: []RejectedAlt{{Option: "Kafka", Why: "operational burden"}},
	})
	s.Record(DecisionRecord{
		Title: "message queue (revisited)", Chosen: "RabbitMQ", Supersedes: []string{d1.ID},
	})
	if !kafkaClosed(s) {
		t.Fatal("superseding with a different choice must leave the rejected option closed")
	}
}

// Scoped: choosing the option in an UNRELATED decision (no supersedes edge to the
// rejecting decision) does NOT un-flag it — the reversal must be tied to the edge.
func TestOverrideReopen_FlagOn_UnrelatedChoiceKeepsClosed(t *testing.T) {
	defer setOverrideReopens(true)()
	s := reopenStore(t)
	s.Record(DecisionRecord{
		Title: "message queue", Chosen: "NATS",
		Rejected: []RejectedAlt{{Option: "Kafka", Why: "operational burden"}},
	})
	// A separate decision elsewhere chooses Kafka, but does NOT supersede D1.
	s.Record(DecisionRecord{Title: "event bus for billing", Chosen: "Kafka"})
	if !kafkaClosed(s) {
		t.Fatal("an unrelated re-choice (no supersedes edge) must not un-flag the rejection")
	}
}
