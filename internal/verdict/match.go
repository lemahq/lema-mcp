// Package verdict holds the shared decision-adjudication judgment used by both
// the MCP check_decided tool (cmd/lema-mcp) and the in-app proposemode tool.
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
	"api": true,
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
