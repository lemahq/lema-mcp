package source

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
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

// TestSupersededByClosesChosen covers a decision condensed from an ADR whose
// successor is a KEPT ADR file (a different source): the loaded record carries
// its own superseded_by, so reduce() must mark it superseded and CLOSE its
// chosen atom — even though no in-store record supersedes it and its written
// status is not "superseded". Without honoring superseded_by, an overridden
// decision would read as in-force.
func TestSupersededByClosesChosen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.jsonl")
	line := `{"id":"ADR-0110","ts":"2026-06-21T00:00Z","title":"Two MCP doors","chosen":"two tools","superseded_by":"ADR-0124","status":"accepted"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewCaptureStore(path)
	if err != nil {
		t.Fatal(err)
	}
	var sawClosedChosen bool
	for _, a := range s.ClosedAtoms() {
		if a.Type == "chosen" && a.Ref == "ADR-0110" {
			sawClosedChosen = true
		}
	}
	if !sawClosedChosen {
		t.Error("superseded_by set should CLOSE the chosen atom even when status != superseded")
	}
}

// TestCaptureRefsSurfaceOnClosedAtom verifies that the provenance an agent
// supplies at record_decision time (file paths, PR URLs) rides through onto the
// CLOSED capture atoms search_decisions / check_decided serve. The WHY this
// matters: a CLOSED "do not propose X" verdict that points at an artifact the
// agent or human can open to confirm the why-NOT is real is followable; an
// unfollowable verdict ("trust me, it's settled") is the uninstall risk — the
// agent can't verify it and a human can't audit it. These atoms come from the
// CAPTURE STORE (.lema/decisions.jsonl); ADR-path CLOSED atoms intentionally
// carry no refs (their provenance is the F1 locator's job), which is why every
// sub-case here sources atoms from a CaptureStore, never the Local ADR index.
func TestCaptureRefsSurfaceOnClosedAtom(t *testing.T) {
	t.Run("chosen and rejected closed atoms carry sanitized refs", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "d.jsonl")
		s, _ := NewCaptureStore(path)
		// Record with provenance, then supersede so the chosen atom is also CLOSED —
		// both the chosen verdict and the rejected alternative must be followable.
		refs := []string{"docs/adr/0099-datastore.md", "https://github.com/lemahq/lema/pull/42"}
		old, err := s.Record(DecisionRecord{
			Title:    "Primary datastore",
			Chosen:   "Postgres",
			Rejected: []RejectedAlt{{Option: "MongoDB", Why: "we need multi-row transactions"}},
			Refs:     refs,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Record(DecisionRecord{
			Title: "Primary datastore v2", Chosen: "CockroachDB", Supersedes: []string{old.ID},
		}); err != nil {
			t.Fatal(err)
		}

		var sawChosen, sawRejected bool
		for _, a := range s.ClosedAtoms() {
			// The superseded chosen atom and the rejected alternative both originate
			// from the record that carried refs; both must surface them.
			if a.Type == "chosen" && strings.Contains(a.Text, "Postgres") {
				sawChosen = true
				assertRefsEqual(t, "superseded chosen", a, refs)
				if !a.Closed {
					t.Error("superseded chosen atom should remain CLOSED")
				}
				if !strings.Contains(a.ClosedNote, "superseded") {
					t.Errorf("superseded chosen lost its ClosedNote: %q", a.ClosedNote)
				}
			}
			if a.Type == "rejected_alternative" && strings.Contains(a.Text, "MongoDB") {
				sawRejected = true
				assertRefsEqual(t, "rejected alternative", a, refs)
				if !a.Closed {
					t.Error("rejected alternative atom should remain CLOSED")
				}
				if !strings.Contains(strings.ToLower(a.ClosedNote), "do not propose") {
					t.Errorf("rejected alternative lost its ClosedNote directive: %q", a.ClosedNote)
				}
			}
		}
		if !sawChosen {
			t.Error("expected the superseded Postgres choice among CLOSED atoms")
		}
		if !sawRejected {
			t.Error("expected the rejected MongoDB alternative among CLOSED atoms")
		}
	})

	t.Run("refs are stripped, truncated, de-duped, and capped before the wire", func(t *testing.T) {
		s, _ := NewCaptureStore(filepath.Join(t.TempDir(), "d.jsonl"))
		long := strings.Repeat("x", 250) // >200 runes — must be hard-cut to 200
		// Construct >8 distinct refs to exercise the cap, plus a control/newline ref,
		// an over-long ref, a duplicate, and an empty-after-trim ref.
		in := []string{
			"  ref\x00with\nctrl  ", // leading/trailing space + NUL + newline → "refwithctrl"
			long,
			"keep-1",
			"keep-1", // exact duplicate → dropped
			"   ",    // empty after trim → dropped
			"keep-2", "keep-3", "keep-4", "keep-5", "keep-6", "keep-7", "keep-8", "keep-9",
		}
		s.Record(DecisionRecord{
			Title: "Caching layer", Chosen: "TanStack Query",
			Rejected: []RejectedAlt{{Option: "SWR", Why: "no mutations"}},
			Refs:     in,
		})

		var alt *Atom
		for _, a := range s.ClosedAtoms() {
			if a.Type == "rejected_alternative" && strings.Contains(a.Text, "SWR") {
				a := a
				alt = &a
				break
			}
		}
		if alt == nil {
			t.Fatal("expected the rejected SWR alternative")
		}
		got := alt.Refs
		// Cap-at-8 is not just a length bound — WHICH refs survive matters: the cap
		// keeps the first 8 by first-occurrence order (after strip/trim/truncate/de-dup),
		// so the agent sees the provenance it listed first, deterministically. Asserting
		// the exact 8-element slice (not just len<=8) pins that order: refwithctrl (#1,
		// control-stripped), the 200-rune truncation of `long` (#2), keep-1 (#3; its #4
		// duplicate and the #5 empty ref are dropped), then keep-2..keep-6 — and keep-7/8/9
		// never make it past the cap.
		assertRefsEqual(t, "capped+ordered refs", *alt, []string{
			"refwithctrl",
			strings.Repeat("x", 200),
			"keep-1", "keep-2", "keep-3", "keep-4", "keep-5", "keep-6",
		})
		if len(got) > 8 {
			t.Errorf("refs not capped at 8: got %d (%v)", len(got), got)
		}
		for _, r := range got {
			if len([]rune(r)) > 200 {
				t.Errorf("ref exceeds 200 runes (%d): %q", len([]rune(r)), r)
			}
			if strings.ContainsAny(r, "\x00\n\t") {
				t.Errorf("ref retained a control character: %q", r)
			}
			if strings.TrimSpace(r) != r || r == "" {
				t.Errorf("ref not trimmed/non-empty: %q", r)
			}
		}
		// The first ref must have its control chars stripped (not replaced).
		if got[0] != "refwithctrl" {
			t.Errorf("control/newline strip wrong: got %q, want %q", got[0], "refwithctrl")
		}
		// The over-long ref is truncated to exactly 200 runes, hard cut (no ellipsis).
		if r := got[1]; len([]rune(r)) != 200 || r != strings.Repeat("x", 200) {
			t.Errorf("long ref not hard-truncated to 200 runes: len=%d", len([]rune(r)))
		}
		// De-dup: "keep-1" appears once.
		count := 0
		for _, r := range got {
			if r == "keep-1" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("duplicate ref not de-duped: keep-1 appears %d times", count)
		}
	})

	t.Run("a record with no refs marshals without a refs key", func(t *testing.T) {
		s, _ := NewCaptureStore(filepath.Join(t.TempDir(), "d.jsonl"))
		s.Record(DecisionRecord{
			Title: "Linter", Chosen: "eslint",
			Rejected: []RejectedAlt{{Option: "tslint", Why: "deprecated"}},
		})
		for _, a := range s.ClosedAtoms() {
			if a.Refs != nil {
				t.Errorf("atom with no captured refs has non-nil Refs: %v", a.Refs)
			}
			b, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "\"refs\"") {
				t.Errorf("omitempty failed — marshaled atom contains a refs key: %s", b)
			}
		}
	})
}

// TestAtomMarshalSizeBounded protects two wire-shape contracts on source.Atom.
// This exists because lema's pitch includes token efficiency (ADR-0040/0036):
// unbounded wire growth on the atom an agent retrieves is a PRODUCT regression,
// not merely a perf one — every byte added to an atom is context the agent pays
// for on every retrieval.
//
//  1. Backward-compat: a refs-less, locator-less atom (the pre-provenance shape —
//     every ADR-path and pre-change capture atom) must marshal with NO "refs" and
//     NO "locator" key, so old atoms serve byte-for-byte as before. omitempty on
//     both fields is the mechanism; this asserts it holds.
//  2. Bounded growth: the only unbounded-looking field is the agent-supplied Refs,
//     but sanitizeRefs hard-caps it at maxRefs (8) × maxRefRunes (200). A fully
//     maxed-out refs payload — 8 refs of 200 runes each — must keep the marshaled
//     refs portion under a sane ceiling. 8×200 runes is at most 8×200×4 bytes of
//     UTF-8 (≈6.4KB) plus JSON punctuation, so ~7KB bounds the wire encoder; we
//     assert under it. A regression that lifts either cap fails here.
//
// Both sub-cases marshal through the SAME shape the production wire uses
// (serve.go's writeJSONResp in github.com/lemahq/lema-mcp: json.NewEncoder + SetEscapeHTML(false)),
// not the default json.Marshal, because they differ exactly where this test's math
// lives: default json.Marshal HTML-escapes <, >, and & to a 6-byte \u00XX each, so
// 200 such chars per ref would marshal to ~9.6KB and demand a ~10KB bound that does
// not describe the wire at all. Marshaling through the wire's non-escaping encoder
// keeps the 6.4KB→7KB math correct and guards the real path the agent retrieves on.
func TestAtomMarshalSizeBounded(t *testing.T) {
	t.Run("refs-less locator-less atom omits both keys", func(t *testing.T) {
		// The exact shape an ADR-path or pre-provenance capture atom serves.
		a := Atom{ID: "d_abc123-chosen", Type: "chosen", Text: "Datastore — chose Postgres", Ref: "d_abc123"}
		b := wireMarshal(t, a)
		if strings.Contains(string(b), "\"refs\"") {
			t.Errorf("omitempty failed — marshaled atom contains a refs key: %s", b)
		}
		if strings.Contains(string(b), "\"locator\"") {
			t.Errorf("omitempty failed — marshaled atom contains a locator key: %s", b)
		}
		// Pin the pre-change wire shape byte-for-byte: this is the backward-compat
		// contract for every atom that predates the provenance fields. wireMarshal
		// trims the encoder's trailing newline so the comparison is to the body bytes.
		const want = `{"id":"d_abc123-chosen","type":"chosen","text":"Datastore — chose Postgres","ref":"d_abc123"}`
		if string(b) != want {
			t.Errorf("pre-provenance wire shape drifted:\n got %s\nwant %s", b, want)
		}
	})

	t.Run("a fully-maxed refs payload stays under the wire ceiling", func(t *testing.T) {
		// Construct refs that survive sanitizeRefs at the maximum: 8 distinct refs,
		// each just over the rune cap so it is truncated to exactly maxRefRunes. Build
		// them from a 4-byte UTF-8 rune (𝕏, U+1D54F) so the maxed payload sits at the
		// genuine worst case (4 bytes/rune) instead of ~5KB under it with ASCII; this
		// makes the assertion exercise the ceiling it claims to guard.
		const worstRune = "𝕏" // U+1D54F, 4 bytes in UTF-8
		raw := make([]string, maxRefs)
		for i := range raw {
			// Distinct prefix keeps them from de-duping; padded past maxRefRunes so each
			// is hard-cut to exactly maxRefRunes runes.
			raw[i] = fmt.Sprintf("ref%d-", i) + strings.Repeat(worstRune, maxRefRunes)
		}
		refs := sanitizeRefs(raw)
		if len(refs) != maxRefs {
			t.Fatalf("expected sanitizeRefs to keep %d refs, got %d", maxRefs, len(refs))
		}
		for i, r := range refs {
			if n := len([]rune(r)); n != maxRefRunes {
				t.Fatalf("ref %d not at the rune cap: len=%d, want %d", i, n, maxRefRunes)
			}
		}
		a := Atom{
			ID: "d_abc123-rej-0", Type: "rejected_alternative",
			Text: "MongoDB — no multi-row transactions", Ref: "d_abc123",
			Refs: refs, Closed: true, ClosedNote: "do not propose MongoDB",
		}
		b := wireMarshal(t, a)
		// Worst case is multi-byte runes (UTF-8 up to 4 bytes/rune): maxRefs*maxRefRunes*4
		// ≈ 6.4KB for the ref strings, plus JSON quotes, commas, brackets, and the "refs":
		// key. Measured at 6483 bytes for this 4-byte-rune payload — under the 7KB ceiling,
		// which bounds the wire encoder (SetEscapeHTML(false)). A cap regression overshoots.
		const ceiling = 7 * 1024
		if len(b) >= ceiling {
			t.Errorf("maxed-out atom marshaled to %d bytes, want < %d (refs cap regression?)", len(b), ceiling)
		}
	})
}

// wireMarshal marshals v exactly as the production MCP wire does
// (serve.go's writeJSONResp in github.com/lemahq/lema-mcp): a json.Encoder with HTML escaping
// turned off. It trims the encoder's trailing newline so callers compare against
// the response body bytes. Using this — not the default json.Marshal — means the
// size bound and the byte-for-byte pin both guard the real serialized path.
func wireMarshal(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// assertRefsEqual checks an atom carries exactly the expected refs, in order.
func assertRefsEqual(t *testing.T, label string, a Atom, want []string) {
	t.Helper()
	if len(a.Refs) != len(want) {
		t.Errorf("%s: refs = %v, want %v", label, a.Refs, want)
		return
	}
	for i := range want {
		if a.Refs[i] != want[i] {
			t.Errorf("%s: refs[%d] = %q, want %q", label, i, a.Refs[i], want[i])
		}
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
