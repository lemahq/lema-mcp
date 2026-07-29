// Package verdict holds the shared decision-adjudication judgment used by both
// the MCP check_decided tool (github.com/lemahq/lema-mcp) and the in-app proposemode tool.
// The matcher here was moved out of package main (was guard_match.go) so both
// surfaces judge through one implementation; parity is then structural.
//
// match.go is the ADR-0053 recall calibration: a distinctiveness-weighted
// matcher that replaces the old all-tokens-AND rule (which required every token
// of a full-sentence option name in the query, so recall collapsed on natural
// language). The principle, precision-first: a match must rest on the killed
// option's DISTINCTIVE terms, never on generic word overlap — because a false
// never-reopen is an uninstall.
//
// Distinctiveness is inverse document frequency over the closed corpus's option
// names: a term in one option (mongodb, pinecone, turborepo) weighs heavily; a
// term in many (or a stopword) weighs ~0. A query's score against an option is
// the summed weight of the distinctive terms they share; it fires above a
// calibrated threshold.
package verdict

import (
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/lemahq/lema-mcp/internal/source"
)

// MatchThreshold is the weighted-overlap floor a query must clear to count as
// reaching a closed option. Calibrated on the eval in match_test.go to fire on
// every labeled positive while leaving every negative empty. Raising it trades
// recall for precision; this value is the recall-max at full precision on that
// set.
const MatchThreshold = 1.5

// distinctMatchFloor / singleTokenTau are the matcher-precision guard against a
// single rare token carrying a match on its own (ADR-0096 follow-up). Weighted IDF
// means one uncommon term (e.g. "ts") can clear MatchThreshold alone, so a query
// about TS *config* lexically reaches a ruling about .ts *imports*: same rare word,
// different approach (a false ruled_out, the costly error). But a single VERY
// distinctive term is sometimes the whole match (a query "switch to Pinecone" vs a
// "Pinecone" rejection) — so we don't drop single-token matches at the lexical
// stage; instead BuildConfirmed demands a STRONGER cosine for them. A genuine
// single-term match is also semantically central (high cosine); an adjacent-topic
// coincidence is marginal. Calibrated against the discussion authority eval.
const (
	distinctMatchFloor = 2    // distinctive shared terms below which the strict cosine applies
	singleTokenTau     = 0.76 // cosine a single-distinctive-term match must clear (vs the normal tau)
	distinctivenessIDF = 2.0  // IDF weight a shared term must clear to COUNT as distinctive (excludes corpus-common words like the project name)
)

// OptionDF is the document frequency of each significant term over the closed
// options' match keys — exported so the confirm stage can reuse the exact
// distinctiveness signal the matcher scores on.
func OptionDF(closed []source.Atom) (df map[string]int, n int) {
	df = map[string]int{}
	for _, a := range closed {
		if a.MatchKey == "" {
			continue
		}
		seen := map[string]bool{}
		for _, t := range significantTokens(matchKeyFor(a)) {
			if !seen[t] {
				seen[t] = true
				df[t]++
			}
		}
	}
	return df, len(closed)
}

// DistinctiveSharedCount is the number of DISTINCT terms a query shares with an
// atom's match key whose IDF over the corpus clears distinctivenessIDF — i.e.
// terms specific enough to carry a real match. A query about TS *config* shares
// only the project name + a near-common word with a ruling about .ts *imports*;
// a genuine rejection shares the approach's several content words. df/n come from
// OptionDF over the same closed set the matcher scored.
func DistinctiveSharedCount(query string, a source.Atom, df map[string]int, n int) int {
	q := map[string]bool{}
	for _, t := range significantTokens(query) {
		q[t] = true
	}
	seen := map[string]bool{}
	cnt := 0
	for _, t := range significantTokens(matchKeyFor(a)) {
		if !q[t] || seen[t] {
			continue
		}
		seen[t] = true
		if w := math.Log((float64(n)+1)/(float64(df[t])+1)) + 1; w >= distinctivenessIDF {
			cnt++
		}
	}
	return cnt
}

// guardStopwords are forced to zero weight regardless of corpus frequency. Two
// classes: function words, and generic engineering/decision terms that carry no
// option IDENTITY. The second class is load-bearing for precision — IDF cannot
// distinguish a generic-but-rare term ("database", appearing in one option) from
// a distinctive-but-rare one ("mongodb"); only this semantic prior can. The
// identity of "Pinecone / dedicated vector database" is "pinecone", not
// "database"; of "MongoDB Atlas Vector Search" is "mongodb", not "vector" or
// "search". Stopwording the category nouns leaves the proper-noun identity to
// carry the match, and keeps "store config in the database" from false-firing.
var guardStopwords = map[string]bool{
	// function words
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"from": true, "into": true, "our": true, "out": true, "all": true, "any": true,
	"via": true, "per": true, "or": true, "in": true, "to": true, "of": true,
	"on": true, "at": true, "by": true, "as": true, "is": true, "are": true,
	"be": true, "we": true, "it": true, "an": true, "if": true, "no": true,
	"do": true, "go": true, "up": true, "its": true, "was": true, "were": true,
	"has": true, "have": true,
	// generic decision verbs / framing
	"use": true, "using": true, "used": true, "approach": true, "option": true,
	"instead": true, "rather": true, "than": true, "should": true, "would": true,
	"could": true, "will": true, "shall": true, "add": true, "adding": true,
	"set": true, "setup": true, "make": true, "let": true, "lets": true,
	"switch": true, "keep": true, "store": true, "stored": true, "think": true,
	"want": true, "need": true, "build": true, "write": true, "only": true,
	"new": true, "old": true, "good": true, "better": true, "improve": true,
	// generic engineering category nouns (never an option identity)
	"data": true, "system": true, "service": true, "support": true, "handle": true,
	"vector": true, "database": true, "search": true, "index": true, "call": true,
	"backend": true, "frontend": true, "tooling": true, "server": true, "client": true,
	"config": true, "app": true, "application": true, "feature": true, "version": true,
	"api": true, "surface": true, "surfaces": true, "tool": true, "tools": true, "authed": true,
	// NB: "code", "embed", "chunks", "retrieval", "document" are deliberately NOT
	// stopworded — they are the only identity the chunk-RAG option has (no proper
	// noun), so the matcher needs them to fire on it.
}

// Tokenize splits s into lowercased alphanumeric tokens, breaking on
// non-alphanumeric runs AND camelCase boundaries, and dropping tokens shorter than
// 2 runes — so "kafka.NewProducer()" and "KafkaBrokers" both yield kafka/new/producer
// and kafka/brokers, and a killed option named inside an identifier still matches.
func Tokenize(s string) []string {
	var toks []string
	var cur []rune
	var prev rune
	flush := func() {
		if len(cur) >= 2 {
			toks = append(toks, strings.ToLower(string(cur)))
		}
		cur = cur[:0]
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			prev = 0
			continue
		}
		// camelCase / digit boundary: a lower/digit run followed by an uppercase
		// letter starts a new token (NewProducer -> new, producer).
		if len(cur) > 0 && unicode.IsUpper(r) && (unicode.IsLower(prev) || unicode.IsDigit(prev)) {
			flush()
		}
		cur = append(cur, r)
		prev = r
	}
	flush()
	return toks
}

// significantTokens returns the option/query tokens that can carry distinctive
// meaning: the tokenizer's output minus stopwords. Deduped.
func significantTokens(s string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range Tokenize(s) {
		if guardStopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// Match returns the CLOSED atoms whose killed option the query reaches, scored
// by shared-distinctiveness and sorted most-specific first. An atom matches when
// the summed IDF weight of the distinctive terms it shares with the query clears
// threshold.
func Match(closed []source.Atom, query string, threshold float64) []source.Atom {
	queryTokens := map[string]bool{}
	for _, t := range significantTokens(query) {
		queryTokens[t] = true
	}
	if len(queryTokens) == 0 {
		return nil
	}

	// Document frequency over the option names: how many distinct closed options
	// contain each token. Rare term → high IDF → distinctive.
	df := map[string]int{}
	keyTokens := make([][]string, len(closed))
	for i, a := range closed {
		if a.MatchKey == "" {
			continue
		}
		toks := significantTokens(matchKeyFor(a))
		keyTokens[i] = toks
		seen := map[string]bool{}
		for _, t := range toks {
			if !seen[t] {
				seen[t] = true
				df[t]++
			}
		}
	}
	n := float64(len(closed))
	weight := func(tok string) float64 {
		// Smoothed IDF: log((N+1)/(df+1)) + 1, floored at 0. A term in every
		// option contributes ~0; a term in one contributes near log(N)+1.
		w := math.Log((n+1)/(float64(df[tok])+1)) + 1
		if w < 0 {
			return 0
		}
		return w
	}

	var out []source.Atom
	for i, a := range closed {
		if a.MatchKey == "" {
			continue
		}
		score := 0.0
		for _, t := range keyTokens[i] {
			if queryTokens[t] {
				score += weight(t)
			}
		}
		if score >= threshold {
			a.Score = score
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// matchKeyFor returns the string the matcher derives terms from: the capture-
// time derived key when present (ADR-0053 — a tight, distinctive key the
// recorder computed), else the raw option name. Letting the derived key win
// means a recorder can sharpen matching without changing the matcher.
func matchKeyFor(a source.Atom) string {
	if a.MatchKeyDerived != "" {
		return a.MatchKeyDerived
	}
	return a.MatchKey
}

// --- Structural gate (2026-07-22, decisions a991ec27 + ea22dd9a) ---
//
// Match is the lexical REACH layer (ADR-0053): it fires whenever the summed
// distinctiveness of a query's shared terms clears MatchThreshold. On the
// check_decided/Build path every closed atom arrives Type="rejected_alternative"
// (Binding force), so a bare lexical reach became a false ruled_out — a meta-topic
// like "add a middle verdict to check_decided" shares this corpus's own vocabulary
// (verdict, closed, atom, decided) or a coincidental rare word (vocabulary, df=1)
// with dozens of atoms it does not govern, and fired the never-reopen on the first.
// Build has no cosine (that is check_approach's semantic gate), so ruled_out is
// gated on lexical STRUCTURE alone.
//
// The signal — calibrated against the real 500-atom binding feed, NOT a synthetic
// fixture — is COVERAGE of the atom's distinctive identity, not per-term rarity: no
// document-frequency bar separates a coincidental rare word from an identity term
// (both can be df=1), but a genuine relitigation NAMES most of an option's
// distinctive terms while a coincidence names a sliver (~2 of an atom's ~11 for the
// specimen). An atom governs iff the query shares >=2 of its distinctive identity
// terms covering >=structuralCov, OR shares one distinctive term that is most of a
// short focused identity (>=structuralCovSolo) — which distinguishes "pinecone"
// (its option's whole identity) from "vocabulary" (one word of a longer, unrelated
// option). "Distinctive" is scale-relative (df <= ~1% of the corpus) and used
// symmetrically in the coverage ratio, so the bar transfers across corpus sizes.
const (
	// distinctiveDFFraction/Floor: an identity term appears in at most
	// max(distinctiveDFFloor, ceil(n*distinctiveDFFraction)) closed options — ~1% of
	// the corpus. The floor keeps a tiny corpus from admitting nothing distinctive.
	distinctiveDFFraction = 0.01
	distinctiveDFFloor    = 2
	// structuralCov: with >=2 shared distinctive terms, the query must cover this
	// share of the atom's distinctive identity. structuralCovSolo: with one shared
	// term, a stricter bar — it must BE most of a focused option's identity.
	structuralCov     = 0.5
	structuralCovSolo = 0.7
)

// distinctiveDF is the highest document frequency a term may have and still count
// as an identity term for the coverage test — scale-relative, floored so a small
// corpus still has a distinctive set.
func distinctiveDF(n int) int {
	c := int(math.Ceil(float64(n) * distinctiveDFFraction))
	if c < distinctiveDFFloor {
		return distinctiveDFFloor
	}
	return c
}

// gateStopwords are function words dropped from the STRUCTURAL GATE's tokenization
// ONLY (not Match's — Match/BuildConfirmed calibration stays byte-identical). They
// are rare in terse option match keys, so IDF would otherwise score them as
// distinctive and let "does"/"not"/"but" carry a false governing match. (The shared
// guardStopwords is missing these; extending it globally is a separate change that
// would perturb Match's calibrated eval.)
var gateStopwords = map[string]bool{
	"does": true, "not": true, "but": true, "when": true, "where": true, "which": true,
	"while": true, "then": true, "them": true, "they": true, "their": true, "there": true,
	"what": true, "who": true, "whom": true, "whose": true, "how": true, "why": true,
	"than": true, "such": true, "some": true, "each": true, "every": true, "both": true,
	"more": true, "most": true, "less": true, "many": true, "much": true, "few": true,
	"one": true, "two": true, "now": true, "here": true, "just": true, "also": true,
	"still": true, "again": true, "same": true, "other": true, "first": true, "last": true,
	"next": true, "yet": true, "already": true, "even": true, "re": true, "so": true,
	"id": true, "v1": true, "over": true, "under": true, "about": true, "across": true,
}

// gateTerms are the tokens the structural gate scores on: significant tokens minus
// gateStopwords, deduped.
func gateTerms(s string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, t := range significantTokens(s) {
		if gateStopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// structuralMatch reports whether the query's lexical hit on atom a is GOVERNING
// (fit to carry ruled_out), not a vocabulary coincidence. df/n come from OptionDF
// over the same closed set the matcher scored (gateStopword df entries are never
// looked up — gateTerms doesn't yield them).
func structuralMatch(query string, a source.Atom, df map[string]int, n int) bool {
	q := map[string]bool{}
	for _, t := range gateTerms(query) {
		q[t] = true
	}
	dd := distinctiveDF(n)
	atomDistinct, shared := 0, 0
	keyTerms, keyShared := 0, 0
	loneShared := ""
	for _, t := range gateTerms(matchKeyFor(a)) {
		// Full-key tally, common terms included — the solo path below needs the
		// identity as WRITTEN, not the post-filter remainder.
		keyTerms++
		if q[t] {
			keyShared++
		}
		if df[t] > dd {
			continue // common in this corpus — category vocabulary, not identity
		}
		atomDistinct++
		if q[t] {
			shared++
			loneShared = t
		}
	}
	if atomDistinct == 0 || shared == 0 {
		return false
	}
	cov := float64(shared) / float64(atomDistinct)
	if shared >= 2 && cov >= structuralCov {
		return true
	}
	if cov < structuralCovSolo {
		return false
	}
	// SOLO PATH — a single distinctive term is carrying the whole match, so cov
	// is 1.0 by construction whenever the df filter stripped every OTHER term of
	// the key. On a long key that ratio measures nothing: the live specimen
	// "Session-source join as MVP path for Judgment pending" reduces to the one
	// term "mvp" (the other six are corpus-common), after which ANY topic naming
	// MVP work reads as governed — measured 2026-07-28, three unrelated topics
	// returned ruled_out/binding against it. Coverage of a one-element set is an
	// artifact of the filter, not evidence of identity.
	//
	// So make the solo bar mean what its comment always claimed — "most of a
	// short focused identity" — by measuring against the identity as written.
	// A genuinely focused option is unaffected ("pinecone" IS its key), and so is
	// verbatim relitigation (the query is the key, so this ratio is 1.0). What it
	// drops is exactly the degenerate case: one common word of a long key.
	//
	// Escape hatch for a token that is an IDENTIFIER rather than a word — naming
	// "adr-0111" or "b0" is naming that thing and nothing else, so it carries a
	// focused identity even inside a long key. Digit-bearing is the only shape
	// signal that SURVIVES Tokenize: underscores, dots and camelCase boundaries
	// all become token splits and case is folded, so "task_id" and "OriginSource"
	// reach this gate as ordinary words ("task", "origin") and cannot be told
	// apart by shape. Measured, not assumed — of the 22 atoms the full-key bar
	// drops, exactly 3 have a digit-bearing lone term.
	if hasDigitRune(loneShared) {
		return true
	}
	return float64(keyShared)/float64(keyTerms) >= structuralCovSolo
}

// hasDigitRune reports whether a gate term carries a digit — the surviving
// identifier signal after Tokenize folds case and splits on punctuation.
func hasDigitRune(s string) bool {
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// Governing returns the CLOSED atoms whose option the query STRUCTURALLY reaches —
// the subset of Match's lexical hits fit to govern a ruled_out, in Match's order.
// check_decided surfaces these (not raw Match) so the atoms it shows and the
// verdict it returns cannot disagree. BuildConfirmed (the server settled path) has
// its own cosine gate and does not go through here.
func Governing(closed []source.Atom, query string) []source.Atom {
	matched := Match(closed, query, MatchThreshold)
	if len(matched) == 0 {
		return matched
	}
	df, n := OptionDF(closed)
	out := make([]source.Atom, 0, len(matched))
	for _, a := range matched {
		if structuralMatch(query, a, df, n) {
			out = append(out, a)
		}
	}
	return out
}
