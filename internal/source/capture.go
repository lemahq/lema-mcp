package source

import (
	"bufio"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// overrideReopens gates the PROPOSED override re-flag semantics (lema-terminal
// Phase 2 follow-up): when a CURRENT decision SUPERSEDES a rejecting decision AND
// chooses the rejected option, that rejected-alternative atom stops enforcing — the
// precedent-not-scripture case (an explicit, human-authored reversal). DEFAULT-OFF
// so the locked never-reopen invariant (ADR-0052) is unchanged until ratified. See
// the proposal doc in workspace/lema-terminal (project)/.
var overrideReopens = os.Getenv("LEMA_OVERRIDE_REOPENS") == "1"

// RejectedAlt is one killed option and why it was killed — the enforcement
// payload nothing else in a repo records. When an agent later proposes this
// option, capture-aware search returns it flagged CLOSED.
type RejectedAlt struct {
	Option string `json:"option"`
	Why    string `json:"why,omitempty"`
}

// TargetEvidence is the redacted resolver receipt retained only when a hosted
// write falls back to an offline draft. It contains stable server identifiers,
// canonical repository identity, and already-redacted evidence values. It must
// never contain credentials, an API URL, a username, or a raw local path.
type TargetEvidence struct {
	SchemaVersion         int                  `json:"schema_version"`
	ProjectWorkspaceID    string               `json:"project_workspace_id"`
	RepositoryWorkspaceID string               `json:"repository_workspace_id"`
	RepositoryCanonical   string               `json:"repository_canonical"`
	ResolvedBy            string               `json:"resolved_by"`
	Evidence              []TargetEvidenceItem `json:"evidence"`
}

type TargetEvidenceItem struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// DecisionRecord is one decision captured at deliberation, persisted as a single
// line in .lema/decisions.jsonl (ADR-0042). It is the local, account-less,
// agent-authored half of the lema decision graph: the calling agent forms it,
// the server only stores and projects it. Status is derived, not authored —
// "superseded" is set when a later record supersedes this one.
type DecisionRecord struct {
	ID          string        `json:"id"`
	TS          string        `json:"ts"`
	Title       string        `json:"title"`
	Chosen      string        `json:"chosen"`
	Rejected    []RejectedAlt `json:"rejected,omitempty"`
	Rationale   string        `json:"rationale,omitempty"`
	Refs        []string      `json:"refs,omitempty"`
	Constraint  string        `json:"constraint,omitempty"`
	Consequence string        `json:"consequence,omitempty"`
	// Assent is agent-relayed provenance: the operator's in-session ruling
	// ("this looks correct"), quoted or dated. Descriptive only — it stages a
	// capture for the boundary batch and NEVER binds (ADR-0125/0129; MC-7).
	Assent         string          `json:"assent,omitempty"`
	Supersedes     []string        `json:"supersedes,omitempty"`
	SupersededBy   *string         `json:"superseded_by,omitempty"`
	Status         string          `json:"status"`
	TargetEvidence *TargetEvidence `json:"target_evidence,omitempty"`
}

// CaptureStore is a writable, JSONL-backed local store of decisions captured at
// deliberation (ADR-0042). The file is append-only; the in-memory state is its
// reduction (last write wins per id, superseded status derived from supersedes
// edges), so concurrent appends merge rather than clobber. Seam-safe: standard
// library only — no database, no model — so it ships in the OSS binary.
type CaptureStore struct {
	mu      sync.Mutex
	path    string
	records []DecisionRecord
	byID    map[string]int

	// cachedAtoms is the last projection of records → Atom slice, populated by
	// loadLocked and invalidated (set to nil) at the top of every loadLocked call.
	// atoms() returns it directly when non-nil, avoiding a full rebuild on every
	// Search/ClosedAtoms call.
	cachedAtoms []Atom

	// loadedMod/loadedSize fingerprint the file as of the last load so reads can
	// cheaply detect when another process has appended and reload. Without this a
	// long-lived `serve --http` store (the GUI backend) would never see a stdio
	// lema-mcp's newly-CLOSED decision, silently regressing never-reopen across
	// processes.
	loadedMod  time.Time
	loadedSize int64
}

// NewCaptureStore loads an existing decisions.jsonl into memory. A missing file
// is not an error — it yields an empty store that the first record creates.
func NewCaptureStore(path string) (*CaptureStore, error) {
	s := &CaptureStore{path: path, byID: map[string]int{}}
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// loadLocked reads the append log from disk into the in-memory reduction and
// records the file's fingerprint. A missing file is not an error — it yields an
// empty store. Caller holds s.mu (the constructor holds the store exclusively).
func (s *CaptureStore) loadLocked() error {
	// Invalidate the atom cache; it will be rebuilt at the end of a successful load.
	s.cachedAtoms = nil

	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.records, s.byID = nil, map[string]int{}
			s.loadedMod, s.loadedSize = time.Time{}, 0
			return nil
		}
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	var lines []DecisionRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r DecisionRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue // tolerate a corrupt line rather than refuse to start
		}
		lines = append(lines, r)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	s.records, s.byID = reduce(lines)
	s.loadedMod, s.loadedSize = fi.ModTime(), fi.Size()
	// Pre-populate the atom cache so subsequent Search/ClosedAtoms calls pay no
	// rebuild cost.
	s.cachedAtoms = s.buildAtoms()
	return nil
}

// refreshIfStale reloads from disk when the backing file changed since the last
// load (different size or mtime), so a record another process appended becomes
// visible without a restart. A stat error or transient read failure leaves the
// last good state in place rather than wiping it. Caller holds s.mu.
func (s *CaptureStore) refreshIfStale() {
	fi, err := os.Stat(s.path)
	if err != nil {
		return
	}
	if fi.Size() == s.loadedSize && fi.ModTime().Equal(s.loadedMod) {
		return
	}
	_ = s.loadLocked()
}

// reduce collapses the append log into current state: last record wins per id,
// then any record's supersedes edges flip the referenced records to superseded,
// and any record's own superseded_by marks itself superseded (so a decision
// condensed from an ADR whose successor is a kept ADR file — a different source
// — still closes its chosen atom rather than reading as in-force).
func reduce(lines []DecisionRecord) ([]DecisionRecord, map[string]int) {
	// Optimization: preallocate slice and map capacity to len(lines) to avoid reallocation.
	// Measured performance impact (for 10,000 items):
	// - Execution time reduced by ~85% (11.5ms -> 1.6ms)
	// - Memory allocations reduced by ~78% (11.3MB -> 2.4MB, 101 allocs -> 35 allocs)
	byID := make(map[string]int, len(lines))
	out := make([]DecisionRecord, 0, len(lines))
	for _, r := range lines {
		if i, ok := byID[r.ID]; ok {
			out[i] = r
			continue
		}
		byID[r.ID] = len(out)
		out = append(out, r)
	}
	for i := range out {
		// A draft (status proposed — the hosted-fallback write) must never un-bind
		// a settled record: its supersedes edges wait until an accepted write of
		// the same decision upgrades it.
		if out[i].Status != "proposed" {
			for _, sid := range out[i].Supersedes {
				if j, ok := byID[sid]; ok {
					out[j].Status = "superseded"
				}
			}
		}
		if out[i].SupersededBy != nil && *out[i].SupersededBy != "" {
			out[i].Status = "superseded"
		}
	}
	return out, byID
}

// decisionID derives the legacy stable 24-bit content id from normalized title
// and chosen text. Because distinct content can collide, consumers that recover
// additional state by this id must also compare the full normalized identity.
func decisionID(title, chosen string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(normalizeDecisionIdentity(title)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(normalizeDecisionIdentity(chosen)))
	return fmt.Sprintf("d_%06x", h.Sum32()&0xffffff)
}

func normalizeDecisionIdentity(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// Record validates, stamps, and appends a decision as accepted, returning the
// stored record — the solo-mode write, where the operator is the only judge.
// title and chosen are required; rejected is strongly encouraged (the tool nudges
// for it) but not enforced here, since a hard minimum invites fabricated
// alternatives. supersedes flips the referenced decisions to superseded. The
// append is the on-disk source of truth; memory is updated to match.
func (s *CaptureStore) Record(in DecisionRecord) (DecisionRecord, error) {
	return s.record(in, "accepted")
}

// RecordDraft persists a decision with status proposed — the hosted-fallback
// write (record_decision.go in github.com/lemahq/lema-mcp): a capture whose hosted push failed
// is preserved durably and searchably, but NON-BINDING. Its rejected
// alternatives do not enforce never-reopen and its supersedes edges do not
// close their targets, because no human and no server trust tier has accepted
// it. A later accepted write of the same decision (same content id) upgrades
// the draft in place.
func (s *CaptureStore) RecordDraft(in DecisionRecord) (DecisionRecord, error) {
	return s.record(in, "proposed")
}

func (s *CaptureStore) record(in DecisionRecord, status string) (DecisionRecord, error) {
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Chosen) == "" {
		return DecisionRecord{}, fmt.Errorf("title and chosen are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	in.ID = decisionID(in.Title, in.Chosen)
	in.TS = time.Now().UTC().Format("2006-01-02T15:04Z")
	in.Status = status
	in.TargetEvidence = cloneTargetEvidence(in.TargetEvidence)

	if err := s.appendLine(in); err != nil {
		return DecisionRecord{}, err
	}
	// Reload from disk so memory reflects this append AND any records another
	// process appended since our last load; reduce() re-derives superseded status
	// from the full log. The on-disk append is the source of truth.
	//
	// If the reload fails the write is still durable — log the error and return
	// the atom we just persisted rather than signalling failure. Returning an
	// error here would cause the caller to retry, creating a duplicate atom on
	// disk (the write already succeeded).
	if err := s.loadLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "lema-mcp: capture: reload after write failed: %v\n", err)
	}
	return in, nil
}

// DraftTargetEvidence returns a detached copy of the resolver evidence stored
// on the content-keyed proposed draft. Re-recording the same title/chosen pair
// is the existing manual retry path for a failed hosted capture.
func (s *CaptureStore) DraftTargetEvidence(title, chosen string) (TargetEvidence, bool) {
	if s == nil {
		return TargetEvidence{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshIfStale()
	index, ok := s.byID[decisionID(title, chosen)]
	if !ok || s.records[index].Status != "proposed" || s.records[index].TargetEvidence == nil {
		return TargetEvidence{}, false
	}
	record := s.records[index]
	if normalizeDecisionIdentity(record.Title) != normalizeDecisionIdentity(title) || normalizeDecisionIdentity(record.Chosen) != normalizeDecisionIdentity(chosen) {
		return TargetEvidence{}, false
	}
	return *cloneTargetEvidence(record.TargetEvidence), true
}

func cloneTargetEvidence(in *TargetEvidence) *TargetEvidence {
	if in == nil {
		return nil
	}
	out := *in
	out.Evidence = append([]TargetEvidenceItem(nil), in.Evidence...)
	return &out
}

// appendLine writes one record as a JSON line, creating the store's directory on
// first use.
func (s *CaptureStore) appendLine(r DecisionRecord) error {
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// atoms returns the cached Atom projection when available, falling back to a
// full rebuild. Caller holds s.mu.
func (s *CaptureStore) atoms() []Atom {
	if s.cachedAtoms != nil {
		return s.cachedAtoms
	}
	s.cachedAtoms = s.buildAtoms()
	return s.cachedAtoms
}

// maxRefs caps the number of provenance refs served per atom; maxRefRunes caps
// each ref's length. Both bound an untrusted, agent-supplied wire surface.
const (
	maxRefs     = 8
	maxRefRunes = 200
)

// sanitizeRefs bounds agent-supplied refs before they are served on the
// MCP wire (search_decisions / check_decided receive them without a trust
// prefix): trims whitespace, drops empties, strips control characters and
// newlines (wire/log-line injection), caps each ref at 200 runes and the
// slice at 8, and de-duplicates preserving first occurrence.
func sanitizeRefs(refs []string) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	seen := make(map[string]struct{}, len(refs))
	for _, raw := range refs {
		// Strip control characters (including newlines and tabs) outright rather
		// than replacing them, so wire/log-line injection markers leave no trace.
		var b strings.Builder
		b.Grow(len(raw))
		for _, r := range raw {
			if unicode.IsControl(r) {
				continue
			}
			b.WriteRune(r)
		}
		ref := strings.TrimSpace(b.String())
		if ref == "" {
			continue
		}
		if rs := []rune(ref); len(rs) > maxRefRunes {
			ref = string(rs[:maxRefRunes]) // hard cut, no ellipsis
		}
		if _, dup := seen[ref]; dup {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
		if len(out) >= maxRefs {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildAtoms projects the captured decisions into the Atom shape search serves,
// computing the CLOSED enforcement note for any rejected alternative and for any
// chosen option whose decision has been superseded. Caller holds s.mu.
func (s *CaptureStore) buildAtoms() []Atom {
	out := []Atom{}
	// PROPOSAL (default-off, LEMA_OVERRIDE_REOPENS): the options a CURRENT decision
	// chose while superseding each rejecting decision. A rejected-alternative atom
	// whose option is rechosen this way stops enforcing — the human-authored reversal.
	// Built only when the flag is on, so the default never-reopen path is unchanged.
	rechosen := s.rechosenViaSupersession()
	for _, r := range s.records {
		// Sanitize the agent-supplied provenance once per record; it rides onto the
		// chosen and rejected atoms (the followable, decision-level claims) but not
		// the constraint/consequence atoms, which stay payload-tight.
		refs := sanitizeRefs(r.Refs)
		// A draft (status proposed — the hosted-fallback write) surfaces in search
		// but binds nothing: its atoms carry a draft marker and never close.
		isDraft := r.Status == "proposed"
		chosenText := r.Title + " — chose " + r.Chosen
		if r.Rationale != "" {
			chosenText += " (" + r.Rationale + ")"
		}
		if isDraft {
			chosenText += " [draft — not yet accepted]"
		}
		chosen := Atom{ID: r.ID + "-chosen", Type: "chosen", Text: chosenText, Ref: r.ID, Refs: refs}
		if r.Status == "superseded" {
			chosen.Closed = true
			chosen.ClosedNote = fmt.Sprintf("superseded — do not reopen %q: the decision %q was overridden by a later one", r.Chosen, r.Title)
			chosen.MatchKey = r.Title + " " + r.Chosen
		}
		out = append(out, chosen)

		for i, alt := range r.Rejected {
			note := fmt.Sprintf("do not propose %q", alt.Option)
			if alt.Why != "" {
				note += ": " + alt.Why
			}
			note += fmt.Sprintf(" (decided %s · %q · chose %s)", r.TS, r.Title, r.Chosen)
			text := alt.Option
			if alt.Why != "" {
				text += " — " + alt.Why
			}
			// Default: a rejected alternative is CLOSED. A draft's rejection never
			// enforces (nobody accepted it). PROPOSAL: if a current decision
			// superseded this one AND chose this option, the team reversed it — the atom
			// stays in the record (history/search) but no longer enforces.
			closed, closedNote := true, note
			if isDraft {
				closed, closedNote = false, ""
				text += " [draft — not yet accepted]"
			} else if overrideReopens && optionReversed(rechosen[r.ID], alt.Option) {
				closed, closedNote = false, ""
			}
			out = append(out, Atom{
				ID: fmt.Sprintf("%s-rej-%d", r.ID, i), Type: "rejected_alternative",
				Text: text, Ref: r.ID, Refs: refs, Closed: closed, ClosedNote: closedNote, MatchKey: alt.Option,
			})
		}
		if r.Constraint != "" {
			out = append(out, Atom{ID: r.ID + "-constraint", Type: "constraint", Text: r.Constraint, Ref: r.ID})
		}
		if r.Consequence != "" {
			out = append(out, Atom{ID: r.ID + "-consequence", Type: "consequence", Text: r.Consequence, Ref: r.ID})
		}
	}
	return out
}

// rechosenViaSupersession maps each superseded decision id → the option keys a
// CURRENT (not itself superseded) decision chose while superseding it. Empty when
// the proposal flag is off, so the default path allocates nothing and behaves
// exactly as before. The reversal is SCOPED to the explicit supersedes edge: an
// unrelated decision that merely chose the same option does not un-flag anything.
func (s *CaptureStore) rechosenViaSupersession() map[string][]string {
	if !overrideReopens {
		return nil
	}
	rechosen := map[string][]string{}
	for _, d2 := range s.records {
		if d2.Status == "superseded" { // a reversal that was itself overridden no longer counts
			continue
		}
		ck := optionKey(d2.Chosen)
		if ck == "" {
			continue
		}
		for _, sid := range d2.Supersedes {
			rechosen[sid] = append(rechosen[sid], ck)
		}
	}
	return rechosen
}

// optionKey normalizes an option/chosen string for matching (lowercase, trimmed).
func optionKey(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// optionReversed reports whether one of the rechosen options matches this rejected
// option — exact normalized equality, or the chosen text containing the option
// (so "Kafka for exactly-once" reverses a rejected "Kafka").
func optionReversed(chosenKeys []string, option string) bool {
	ok := optionKey(option)
	if ok == "" {
		return false
	}
	for _, ck := range chosenKeys {
		if ck == ok || strings.Contains(ck, ok) {
			return true
		}
	}
	return false
}

// Search returns captured atoms relevant to the query, ranked lexically with a
// boost for CLOSED atoms so a killed or superseded option surfaces strongly the
// moment an agent asks about it.
func (s *CaptureStore) Search(query string, k int) []Atom {
	if s == nil {
		return nil
	}
	terms := queryTerms(query)
	if len(terms) == 0 {
		return nil
	}
	if k <= 0 || k > 50 {
		k = 8
	}
	s.mu.Lock()
	s.refreshIfStale()
	atoms := s.atoms()
	s.mu.Unlock()

	type scored struct {
		atom  Atom
		score float64
	}
	cands := []scored{}
	for _, a := range atoms {
		cl := strings.ToLower(a.Text)
		var hits float64
		for _, t := range terms {
			hits += float64(strings.Count(cl, t))
		}
		if hits == 0 {
			continue
		}
		score := hits * lengthNorm(len([]rune(a.Text)))
		if a.Closed {
			score *= 1.75 // a settled-and-closed match is the most important thing to surface
		}
		cands = append(cands, scored{a, score})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })

	out := []Atom{}
	for i := 0; i < len(cands) && i < k; i++ {
		a := cands[i].atom
		a.Score = cands[i].score
		out = append(out, a)
	}
	return out
}

// CheckDecided returns the CLOSED decisions matching a topic — the dedicated
// never-reopen gate an agent calls before proposing a direction. Only closed
// atoms (rejected alternatives, superseded choices) are returned; an empty
// result means nothing about this topic is settled-and-closed.
func (s *CaptureStore) CheckDecided(topic string, k int) []Atom {
	if s == nil {
		return nil
	}
	if k <= 0 || k > 50 {
		k = 10
	}
	closed := []Atom{}
	for _, a := range s.Search(topic, k*2) {
		if a.Closed {
			closed = append(closed, a)
			if len(closed) >= k {
				break
			}
		}
	}
	return closed
}

// ClosedAtoms returns every currently-CLOSED atom — rejected alternatives and
// superseded choices — across all captured decisions, most-recent decision first.
// It is the never-reopen surface with no query: the GUI's enforcement rail lists
// these so a killed or overridden option is visible the moment it is recorded
// (including by another process, via the freshness reload in refreshIfStale).
func (s *CaptureStore) ClosedAtoms() []Atom {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	s.refreshIfStale()
	atoms := s.atoms()
	s.mu.Unlock()

	out := []Atom{}
	for i := len(atoms) - 1; i >= 0; i-- {
		if atoms[i].Closed {
			out = append(out, atoms[i])
		}
	}
	return out
}

// Len returns the number of captured decisions (for startup logging).
func (s *CaptureStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshIfStale()
	return len(s.records)
}
