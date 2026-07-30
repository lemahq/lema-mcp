// guard_precision_eval_test.go measures the never-reopen guard in BOTH
// directions against the real binding feed.
//
// It exists because pain-point #27 recorded eight false `ruled_out` fires in one
// working session, and because d_23bf88 already established that a synthetic
// fixture is scale-lockable to whatever makes it pass — so the eval must run on
// real corpus data or it proves nothing.
//
//	PRECISION: hand-labelled negatives (testdata/guard_specimens.json) are
//	verbatim excerpts of documents that really tripped the guard. Each must not
//	fire. These are the regression pins.
//
//	RECALL: generated from the corpus itself — a query naming a killed option
//	verbatim IS a relitigation and MUST fire. Measured over every atom, so a
//	precision fix that quietly guts the guard cannot pass.
//
// Skipped unless LEMA_GUARD_EVAL_CORPUS points at a closed-atom feed
// (.lema/closed.hosted.json), because the feed is not this repo's to vendor.
//
//	LEMA_GUARD_EVAL_CORPUS=~/Projects/lemahq/lema/.lema/closed.hosted.json \
//	  go test ./cmd/lema-mcp/ -run TestGuardPrecisionEval -v
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

type guardSpecimen struct {
	Name            string `json:"name"`
	WronglyFired    string `json:"wrongly_fired"`
	WronglyFiredKey string `json:"wrongly_fired_key"`
	// Baseline is the target file's content BEFORE the edit, when the specimen
	// came from editing an existing file. It feeds the novelty gate, which is
	// the only thing that can answer "did this edit introduce the name?" — an
	// identifier already in the file is carried along, not proposed. Empty
	// means a new file (or plain prose), where the lexical gate stands alone.
	Baseline string `json:"baseline"`
	// KnownUnfixedReason, when set, marks a specimen the matcher provably
	// cannot reach. It is REPORTED, never silently tolerated, and a specimen
	// that starts passing must have this cleared so the file stays truthful.
	KnownUnfixedReason string `json:"known_unfixed_reason"`
	WhyFalse           string `json:"why_false"`
	Text               string `json:"text"`
}

// guardPositive is a hand-labelled specimen that MUST fire: a real attempt to
// introduce a surface named after a killed option. These exist because a
// precision fix can always be made to pass by matching less, and the generated
// recall set below only ever exercises one sentence shape ("let's use X"). A
// declaration, a route, and two prose proposals pin the shapes a real naming
// proposal actually takes.
type guardPositive struct {
	Name        string `json:"name"`
	MustFireKey string `json:"must_fire_key"`
	WhyTrue     string `json:"why_true"`
	Baseline    string `json:"baseline"`
	Text        string `json:"text"`
}

type guardSpecimenFile struct {
	Negatives         []guardSpecimen `json:"negatives"`
	Positives         []guardPositive `json:"positives"`
	PositiveTemplates []string        `json:"positive_templates"`
}

// evalHits is the specimen-side equivalent of the hook's evaluation order:
// the pure matcher, then the novelty gate against the pre-edit file. Keeping
// them in one helper is what stops the eval from measuring a pipeline the
// guard does not actually run.
func evalHits(corpus []source.Atom, text, baseline string) []source.Atom {
	return guardNovelHits(guardMatch(corpus, text), baseline)
}

// loadEvalCorpus reads the real closed-atom feed THROUGH THE GUARD'S OWN CACHE
// DTO. This is load-bearing, not incidental: source.Atom deliberately omits
// MatchKey/MatchKeyDerived from its JSON wire (guard_cache.go:55-58), so
// unmarshalling the feed straight into []source.Atom yields 500 atoms with
// empty keys — matchClosed then skips every one and the whole eval passes
// vacuously. The first draft of this file did exactly that and reported a
// meaningless green.
func loadEvalCorpus(t *testing.T) []source.Atom {
	t.Helper()
	path := os.Getenv("LEMA_GUARD_EVAL_CORPUS")
	if path == "" {
		t.Skip("set LEMA_GUARD_EVAL_CORPUS to a closed-atom feed to run the guard eval")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var c struct {
		Atoms []guardCacheAtom `json:"atoms"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}
	atoms := make([]source.Atom, 0, len(c.Atoms))
	for _, a := range c.Atoms {
		atoms = append(atoms, a.toAtom())
	}
	keyed := 0
	for _, a := range atoms {
		if a.MatchKey != "" {
			keyed++
		}
	}
	if keyed == 0 {
		t.Fatalf("corpus loaded %d atoms but none carry a match_key — the eval would pass vacuously", len(atoms))
	}
	return atoms
}

func loadGuardSpecimens(t *testing.T) guardSpecimenFile {
	t.Helper()
	b, err := os.ReadFile("testdata/guard_specimens.json")
	if err != nil {
		t.Fatalf("read specimens: %v", err)
	}
	var f guardSpecimenFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("parse specimens: %v", err)
	}
	return f
}

// TestGuardPrecisionEval is the measurement. It reports both directions and
// fails on a false fire; recall is asserted against a floor so that a precision
// change which guts the guard cannot pass silently.
func TestGuardPrecisionEval(t *testing.T) {
	corpus := loadEvalCorpus(t)
	spec := loadGuardSpecimens(t)
	t.Logf("corpus: %d closed atoms", len(corpus))

	// --- PRECISION: real documents that must not fire ---
	falseFires, knownUnfixed := 0, 0
	for _, neg := range spec.Negatives {
		hits := evalHits(corpus, neg.Text, neg.Baseline)
		fired := guardFires(hits)

		if !fired {
			if neg.KnownUnfixedReason != "" {
				t.Errorf("SPECIMEN STALE %-24s is marked known-unfixed but now passes — clear known_unfixed_reason so this file stays truthful", neg.Name)
				continue
			}
			t.Logf("PASS precision %-26s no fire", neg.Name)
			continue
		}

		var named []string
		for _, h := range hits {
			if len(named) < 4 {
				named = append(named, fmt.Sprintf("%s(%q, score %.0f)", h.Ref, h.MatchKey, h.Score))
			}
		}
		if neg.KnownUnfixedReason != "" {
			knownUnfixed++
			t.Logf("KNOWN-UNFIXED %-25s still fires: %s\n    %s", neg.Name, strings.Join(named, ", "), neg.KnownUnfixedReason)
			continue
		}
		falseFires++
		t.Errorf("FALSE FIRE %-26s expected silence, got %d hit(s): %s\n    why false: %s",
			neg.Name, len(hits), strings.Join(named, ", "), neg.WhyFalse)
	}

	// --- POSITIVES: a real attempt to introduce the surface must still fire ---
	missedPositives := 0
	for _, pos := range spec.Positives {
		hits := evalHits(corpus, pos.Text, pos.Baseline)
		hit := false
		for _, h := range hits {
			if strings.EqualFold(h.MatchKey, pos.MustFireKey) {
				hit = true
				break
			}
		}
		if hit && guardFires(hits) {
			t.Logf("PASS positive  %-26s fires on %q", pos.Name, pos.MustFireKey)
			continue
		}
		missedPositives++
		t.Errorf("MISSED POSITIVE %-23s expected a fire on %q, got %d hit(s)\n    why true: %s",
			pos.Name, pos.MustFireKey, len(hits), pos.WhyTrue)
	}

	// --- RECALL: naming a killed option verbatim must fire it ---
	tmpl := "let's use %s"
	if len(spec.PositiveTemplates) > 0 {
		tmpl = spec.PositiveTemplates[0]
	}
	var missed []string
	eligible := 0
	for _, a := range corpus {
		if a.MatchKey == "" {
			continue
		}
		eligible++
		hits := guardMatch(corpus, fmt.Sprintf(tmpl, a.MatchKey))
		fired := false
		for _, h := range hits {
			if h.ID == a.ID {
				fired = true
				break
			}
		}
		if !fired || !guardFires(hits) {
			missed = append(missed, fmt.Sprintf("%s(%q)", a.Ref, a.MatchKey))
		}
	}
	recall := 100.0
	if eligible > 0 {
		recall = 100.0 * float64(eligible-len(missed)) / float64(eligible)
	}

	t.Logf("PRECISION: %d/%d labelled negatives silent (%d known-unfixed, %d regressions)",
		len(spec.Negatives)-falseFires-knownUnfixed, len(spec.Negatives), knownUnfixed, falseFires)
	t.Logf("POSITIVES: %d/%d labelled naming proposals still fire", len(spec.Positives)-missedPositives, len(spec.Positives))
	t.Logf("RECALL:    %.1f%% (%d/%d killed options fire when named verbatim)", recall, eligible-len(missed), eligible)
	if len(missed) > 0 {
		sort.Strings(missed)
		show := missed
		if len(show) > 12 {
			show = show[:12]
		}
		t.Logf("  missed (%d): %s", len(missed), strings.Join(show, ", "))
	}

	// A guard that stops firing is not a fix. Recall is asserted, not observed.
	const recallFloor = 95.0
	if recall < recallFloor {
		t.Errorf("recall %.1f%% below floor %.1f%% — a precision change must not gut the guard", recall, recallFloor)
	}
}

// TestGuardShortKeyExposure reports how much of the corpus is structurally
// exposed to the pain-point #27 failure: an option whose whole identity is one
// or two ordinary words fires on any prose containing them. Diagnostic only —
// it asserts nothing, so it cannot block a corpus that legitimately grows.
func TestGuardShortKeyExposure(t *testing.T) {
	corpus := loadEvalCorpus(t)
	var short []string
	for _, a := range corpus {
		if a.MatchKey == "" {
			continue
		}
		if n := len(strings.Fields(a.MatchKey)); n > 0 && n <= 2 {
			short = append(short, fmt.Sprintf("%s(%q)", a.Ref, a.MatchKey))
		}
	}
	sort.Strings(short)
	t.Logf("%d/%d atoms (%.0f%%) have a 1-2 word match key:\n  %s",
		len(short), len(corpus), 100*float64(len(short))/float64(len(corpus)), strings.Join(short, "\n  "))
}
