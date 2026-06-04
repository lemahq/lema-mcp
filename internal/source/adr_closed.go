package source

import (
	"fmt"
	"strings"

	"github.com/lemahq/lema-mcp/internal/adr"
)

// rejectedAlt is one option an ADR documented and rejected — the option name plus
// the recorded reason. The name is the never-reopen match key (ADR-0053).
type rejectedAlt struct {
	name   string
	reason string
}

// rejectedAlternatives extracts the rejected options from an ADR body's
// "Alternatives considered" section: each `### <name>` subsection under it whose
// text marks a "Status: rejected" line. The subsection heading is the option name;
// the status line is the reason. Prose-only alternatives (no `###` + explicit
// rejected status) yield nothing — without a clean option name there is no precise
// match, and a fuzzy one would re-create the false-positive problem ADR-0052 fixed.
func rejectedAlternatives(body string) []rejectedAlt {
	var out []rejectedAlt
	inAlts := false
	for _, s := range splitSections(body) {
		if s.level == 2 {
			inAlts = sectionType(s.heading) == "rejected"
			continue
		}
		if !inAlts || s.level != 3 {
			continue
		}
		if reason, ok := rejectedStatus(s.text); ok {
			if name := cleanOptionName(s.heading); name != "" {
				out = append(out, rejectedAlt{name: name, reason: reason})
			}
		}
	}
	return out
}

// rejectedStatus returns the reason and true if the subsection text carries a
// "Status: rejected" line (bold or plain); false for chosen / no status.
func rejectedStatus(text string) (string, bool) {
	for _, ln := range strings.Split(text, "\n") {
		low := strings.ToLower(ln)
		if !strings.Contains(low, "status") || !strings.Contains(low, "rejected") {
			continue
		}
		i := strings.Index(low, "rejected")
		reason := strings.TrimSpace(ln[i+len("rejected"):])
		reason = strings.TrimSpace(strings.TrimLeft(reason, " —-:"))
		return reason, true
	}
	return "", false
}

// cleanOptionName strips a trailing parenthetical (e.g. " (chosen)") from an
// alternative's heading so the match key is just the option name.
func cleanOptionName(h string) string {
	if i := strings.LastIndex(h, " ("); i > 0 {
		h = h[:i]
	}
	return strings.TrimSpace(h)
}

// ClosedSource is the optional capability a DecisionSource exposes when it can
// surface CLOSED no-go atoms for never-reopen enforcement. The local ADR source
// implements it (ADR-0053); the hosted source may later.
type ClosedSource interface {
	ClosedAtoms() []Atom
}

// ClosedAtoms returns the CLOSED no-go atoms documented in accepted, non-superseded
// ADRs: each rejected alternative becomes an atom whose MatchKey is the option name,
// so ADR-0052's option-token matcher enforces it (ADR-0053). Superseded / deprecated
// / rejected / proposed ADRs and any with `superseded_by` set contribute nothing —
// a rejection a later decision reopened is never enforced.
func (l *Local) ClosedAtoms() []Atom {
	out := []Atom{}
	for _, n := range l.order {
		a := l.byNum[n]
		if !adrEnforceable(a) {
			continue
		}
		ref := a.Ref
		if ref == "" {
			ref = fmt.Sprintf("ADR-%04d", a.Number)
		}
		for i, alt := range rejectedAlternatives(a.Body) {
			note := fmt.Sprintf("do not propose %q", alt.name)
			if alt.reason != "" {
				note += ": " + alt.reason
			}
			note += fmt.Sprintf(" (%s · %q)", ref, a.Title)
			text := alt.name
			if alt.reason != "" {
				text += " — " + alt.reason
			}
			out = append(out, Atom{
				ID:         fmt.Sprintf("%s-rej-%d", ref, i),
				Type:       "rejected_alternative",
				Text:       text,
				Ref:        ref,
				Closed:     true,
				ClosedNote: note,
				MatchKey:   alt.name,
			})
		}
	}
	return out
}

// adrEnforceable reports whether an ADR's rejected alternatives should enforce:
// status "accepted" and no supersession (ADR-0053). Proposed/superseded/deprecated/
// rejected ADRs do not enforce.
func adrEnforceable(a adr.ADR) bool {
	if a.SupersededBy != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.Status), "accepted")
}
