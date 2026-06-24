package verdict

import (
	"fmt"
	"strings"

	"github.com/lemahq/lema-mcp/internal/source"
)

// Label is the adjudication outcome an agent acts on.
type Label string

const (
	RuledOut    Label = "ruled_out"     // a binding closed decision governs this topic — do not re-propose
	NotRuledOut Label = "not_ruled_out" // closed set acquired, no binding match (advisory/historical may still be surfaced)
	Incomplete  Label = "incomplete"    // the closed set could not be fully acquired — NOT a confident no-match
	Errored     Label = "error"         // hard failure acquiring the closed set
)

// Force is how forcefully a governing decision binds. Derived, not stored.
type Force string

const (
	Binding    Force = "binding"    // rejected alternative of a live decision — a hard stop
	Advisory   Force = "advisory"   // weaker: closed without the strength of a binding rejection
	Historical Force = "historical" // superseded/deprecated lineage — context, not a live constraint
)

// GoverningDecision is one closed atom that governs the checked topic.
type GoverningDecision struct {
	Ref          string  `json:"ref"`
	DerivedForce Force   `json:"derived_force"`
	Summary      string  `json:"summary,omitempty"`
	Score        float64 `json:"-"`
}

// Verdict is the typed adjudication returned to the agent.
type Verdict struct {
	Verdict            Label               `json:"verdict"`
	GoverningDecisions []GoverningDecision `json:"governing_decisions"`
	Reason             string              `json:"reason"`
}

// DeriveForce tags a CLOSED atom by how forcefully it governs, from the fields
// the atom already carries (no schema add). A rejected alternative of a live
// decision is binding; a superseded/deprecated lineage — and a CLOSED chosen
// direction, which the capture store marks closed only when its decision was
// superseded (source/capture.go) — is historical context. An atom whose type we
// can't read FAILS ADVISORY: surfaced as context, never as a binding hard stop.
// The write path (ADR-0124/0125) inverted the older "default to binding is the
// safe, non-muting fallback" rule: once any principal can push atoms into the
// corpus, a forged or garbled closed atom that auto-binds is the gun, so the
// binding force is reserved for an explicitly rejected alternative and everything
// unrecognized degrades to advisory. NB: the hosted feed sets
// Type="rejected_alternative" in ClosedAtomsAsSource (repo/closed_atoms.go), so
// hosted atoms resolve to Binding via the explicit case below — this fallback
// only governs atoms whose type genuinely could not be read.
func DeriveForce(a source.Atom) Force {
	switch strings.ToLower(a.Type) {
	case "rejected", "rejected_alternative":
		return Binding
	case "superseded", "deprecated":
		return Historical
	case "chosen":
		// A CLOSED chosen direction is a superseded lineage (the capture store
		// closes it only on supersession) — historical context, not a hard
		// ruling. Its rejected ALTERNATIVES carry the no-go force.
		if a.Closed {
			return Historical
		}
		return Advisory
	case "advisory":
		return Advisory
	default:
		// Fail advisory: do not manufacture a hard stop from a type we cannot
		// read, whether or not the atom is flagged closed.
		return Advisory
	}
}

func refOf(a source.Atom) string {
	if a.Locator != "" {
		return a.Locator
	}
	if a.Ref != "" {
		return a.Ref
	}
	return a.MatchKey
}

// Build judges a topic against an already-acquired, already-scoped CLOSED set.
// ruled_out requires a binding match — the only hard stop; advisory/historical
// matches are surfaced as context under not_ruled_out (the anti-mute rule: an
// advisory-only hit must not read as a hard stop). For the partial/error cases
// the caller uses NewIncomplete / NewErrored instead, so the verb fails loud.
func Build(closed []source.Atom, topic string) Verdict {
	return buildFrom(Match(closed, topic, MatchThreshold))
}

// BuildConfirmed is Build plus a semantic gate (ADR-0096): of the lexical matches,
// keep only those whose query-similarity sim[atom.ID] >= tau. This is the matcher-
// precision fix — a lexical token-overlap that is NOT the same option (low cosine)
// no longer fires a false "settled". sim is supplied by the caller (server-side
// embed + cosine); atoms absent from sim are treated as unconfirmed (dropped).
// tau MUST be > 0: a missing sim key reads as 0.0, so a non-positive tau would
// invert the rule and let unconfirmed atoms through.
func BuildConfirmed(closed []source.Atom, topic string, sim map[string]float64, tau float64) Verdict {
	matched := Match(closed, topic, MatchThreshold)
	df, n := OptionDF(closed)
	confirmed := make([]source.Atom, 0, len(matched))
	for _, a := range matched {
		// A match resting on fewer than distinctMatchFloor DISTINCTIVE shared terms
		// must clear a stronger cosine: a query about TS config reaching a ruling
		// about .ts imports shares only near-common words (a marginal cosine, the
		// false ruled_out), while a genuine single-term match is semantically central
		// (high cosine). Multi-distinctive-term matches use the normal tau.
		effTau := tau
		if DistinctiveSharedCount(topic, a, df, n) < distinctMatchFloor && singleTokenTau > tau {
			effTau = singleTokenTau
		}
		if sim[a.ID] >= effTau {
			confirmed = append(confirmed, a)
		}
	}
	return buildFrom(confirmed)
}

// buildFrom turns an already-matched (and possibly already-confirmed) set into the
// typed verdict. Shared by Build and BuildConfirmed so their envelope logic cannot
// drift.
func buildFrom(matched []source.Atom) Verdict {
	if len(matched) == 0 {
		return Verdict{Verdict: NotRuledOut, GoverningDecisions: []GoverningDecision{},
			Reason: "no governing decision found for this topic"}
	}
	gov := make([]GoverningDecision, 0, len(matched))
	hasBinding := false
	for _, a := range matched {
		f := DeriveForce(a)
		if f == Binding {
			hasBinding = true
		}
		gov = append(gov, GoverningDecision{Ref: refOf(a), DerivedForce: f, Summary: a.ClosedNote, Score: a.Score})
	}
	if hasBinding {
		return Verdict{Verdict: RuledOut, GoverningDecisions: gov,
			Reason: fmt.Sprintf("ruled out by %s — do not re-propose; surface the prior decision instead", refOf(matched[0]))}
	}
	return Verdict{Verdict: NotRuledOut, GoverningDecisions: gov,
		Reason: "not a hard ruling; advisory or historical context exists — proceed with awareness"}
}

// NewIncomplete is the honest verdict when the closed set is unsynced/partial —
// never collapse this to not_ruled_out.
func NewIncomplete(reason string) Verdict {
	return Verdict{Verdict: Incomplete, GoverningDecisions: []GoverningDecision{}, Reason: reason}
}

// NewErrored is the fail-loud verdict when the closed set could not be acquired.
func NewErrored(reason string) Verdict {
	return Verdict{Verdict: Errored, GoverningDecisions: []GoverningDecision{}, Reason: reason}
}
