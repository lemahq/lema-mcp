package verdict

import (
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// Regression pins for the false `ruled_out` verdicts of 2026-07-25/26.
//
// A false ruled_out is the costly error in both directions: a MISS costs a
// re-litigated decision, but a FALSE POSITIVE stops correct work with fabricated
// authority and teaches agents to ignore the guard — which destroys the thing lema
// exists to provide. Each case below therefore asserts WHY the match is wrong: the
// query and the atom sit in different SUBJECT DOMAINS and share only a category
// noun. Pinning the output alone would let a future matcher get the right answer
// for a reason that does not generalise.

// TestFalseRuledOut_CacheDomainBoundary is misfire 1, reproduced verbatim.
//
// Topic: Docker BUILD-LAYER caching in CI (buildx, type=registry vs type=gha).
// Atom:  ADR-0055's "Index only, no content cache" — caching document CONTENT for
//
//	agent retrieval.
//
// These are unrelated engineering domains. The ONLY shared token is the category
// noun "cache". The mechanism that made it binding: the structural gate scored
// coverage over the atom's corpus-RARE residue rather than its stated identity, so
// "Index only, no content cache" collapsed to {cache} ("content" was df=11, above
// the distinctiveness cutoff, and was dropped from the DENOMINATOR instead of
// counted as unmatched). One shared word then scored 100% coverage.
func TestFalseRuledOut_CacheDomainBoundary(t *testing.T) {
	// Mirrors the real corpus shape: "content" recurs across many option keys
	// (category vocabulary), "cache" is rarer but still not an identity.
	corpus := []source.Atom{
		rejAtom("ADR-0055", "Index only, no content cache"),
	}
	for i := 0; i < 12; i++ {
		corpus = append(corpus, rejAtom("filler", "content pipeline stage"+string(rune('a'+i))))
	}
	for i := 0; i < 3; i++ {
		corpus = append(corpus, rejAtom("cachefill", "cache warmer variant"+string(rune('a'+i))))
	}

	topic := "registry-backed docker buildx layer cache (type=registry) instead of type=gha"
	if v := Build(corpus, topic); v.Verdict == RuledOut {
		t.Errorf("FALSE ruled_out: a CI build-layer-cache topic is not governed by a "+
			"document-content-cache ruling; they share only the category noun \"cache\". got %s (%s)",
			v.Verdict, v.Reason)
	}
}

// TestBuildGate_SoloTermNoLongerBinds documents the trade that fixing the above
// required, so it cannot be reversed by accident.
//
// Sharing ONE distinctive term with an option no longer reaches ruled_out; the
// query must cover the option's STATED identity. This costs partial-naming recall
// (measured on the real 639-atom feed: 100% -> 86.3% when the query names only an
// option's distinctive terms), and the loss is accepted deliberately because the
// binding verdict is the one place precision outranks recall.
//
// A df-based exemption for "solo terms that look like proper nouns" was tried and
// REMOVED the same day: "registry" is df-rare among option keys, so a Docker-registry
// topic once again governed a PACKAGE-RESOLVER registry ruling. That is d_23bf88
// ("no df bar separates coincidental-rare from identity-rare") re-derived a third
// time. Do not re-propose it.
func TestBuildGate_SoloTermNoLongerBinds(t *testing.T) {
	corpus := syntheticGateCorpus()
	if v := Build(corpus, "let us reconsider ident7 for the redesign"); v.Verdict == RuledOut {
		t.Errorf("a single shared identity term must not BIND on its own; got %s", v.Verdict)
	}
	// ...but naming the option's stated identity still binds — the guard is not gutted.
	if v := Build(corpus, "let us reconsider the ident7 ledger pipeline"); v.Verdict != RuledOut {
		t.Errorf("naming the option's stated identity must still fire; got %s", v.Verdict)
	}
}

// TestStructuralCoverageUsesStatedIdentity pins the mechanism directly: a common
// term inside an option name must count against coverage, not vanish from it.
// Without this, precision is inversely proportional to identity strength — the
// weaker an option's identity, the easier it is to "fully cover".
func TestStructuralCoverageUsesStatedIdentity(t *testing.T) {
	corpus := []source.Atom{rejAtom("a", "alpha common common common")}
	for i := 0; i < 20; i++ {
		corpus = append(corpus, rejAtom("f", "common filler"+string(rune('a'+i))))
	}
	df, n := OptionDF(corpus)
	a := corpus[0]

	// Sharing only the rare term covers 1 of 4 stated terms — not a governing match.
	if structuralMatch("we should try alpha here", a, df, n) {
		t.Error("one shared term out of four stated must not govern")
	}
	// Sharing the whole stated identity does.
	if !structuralMatch("bring back alpha common common common", a, df, n) {
		t.Error("covering the option's stated identity must govern")
	}
}
