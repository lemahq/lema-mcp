// guard_naming.go narrows the never-reopen guard's SINGLE-TOKEN match keys so
// they fire on a naming PROPOSAL rather than on any occurrence of the word.
//
// THE DEFECT. optionMatches resolves a one-word killed option by bare token-set
// membership, and verdict.Tokenize splits camelCase and underscores. So the
// option "Record" (d_e45c79 — the rejected name for the org-wide knowledge
// destination) matched `PublicRecord`, `GetRepoRecord`,
// `publicRepoRecordResponse` and `public_record.go`, and "Knowledge" matched the
// index name `jobs_one_active_knowledge_audit`. Every one of those is a
// long-standing identifier, none is a naming proposal — measured: the guard
// fired on EVERY edit to apps/api/internal/{repo,api}/public_record.go in one
// session. A guard that cries wolf on routine work gets clicked through, which
// costs exactly the cases it exists to catch (the zero-FP contract in
// decisioncheck's package doc).
//
// WHY NOT THE PREVIOUSLY RECORDED REMEDY. d_194bff records this class as "a DATA
// problem, not a matcher problem", to be fixed by a sharper ADR-0053
// match_key_derived. That holds for a VERBOSE key (sharpen 14 terms to their
// identity core) but is structurally unavailable here: these atoms come from a
// NAMING decision, where the rejected option IS one ordinary English word.
// There is no sharper key for "Record" — the ambiguity is the name itself. The
// separating information is not in the key, it is in HOW THE WORD OCCURS, which
// only the matcher sees. d_194bff's other finding still binds and is honored:
// "suppress single-token keys entirely" stays rejected, because a one-word
// product name (Kafka, Docusaurus) is the guard's best case.
//
// THE GATE. Applies ONLY to single-token keys — the exposed class (3 of 500
// corpus atoms). Multi-word keys keep the contiguous-run adjacency of
// pain-point #27 untouched. Three questions, each of which can only SUPPRESS,
// and each on a closed class rather than a tunable statistic (d_23bf88: a
// threshold tuned to make specimens pass is scale-lockable to the fixture):
//
//  1. POSITION — is the word its own lexical unit, or a fragment of a longer
//     identifier? `PublicRecord`/`public_record` qualify "Record" with something
//     else; `KafkaClient` names the compound AFTER Kafka. So an occurrence
//     counts when nothing identifier-ish precedes it; a non-initial fragment
//     never does.
//  2. CASE — a capitalized key is a proper name, and English proper names are
//     capitalized. Lowercase `record` in prose is the common noun. Waived in
//     code positions, where lowercasing a product name is conventional
//     (`kafka.go`, `kafka.NewProducer()`, `/record`, `import kafka`) — this is
//     why case could not be applied bluntly (the pinned kafka behaviour).
//  3. DETERMINER — English proper names reject articles: "the Record" is a noun
//     phrase USING the chosen name, "use Record" MENTIONS the killed one. This
//     is the use/mention distinction, not a frequency heuristic.
//
// Case 3's residual is deliberate and documented: "add a Record surface" reads
// identically to "into the Record" and is suppressed. Firing on every "the
// record" in the corpus is the worse trade.
package main

import (
	"io"
	"os"
	"strings"
	"unicode"

	"github.com/lemahq/lema-mcp/internal/source"
	"github.com/lemahq/lema-mcp/internal/verdict"
)

// guardDeterminers are the English function words that mark the following noun
// as a USE inside a noun phrase rather than a MENTION of a name. Closed class
// by construction — this is the article/demonstrative/possessive set, not a
// tunable stopword list that grows with the corpus.
var guardDeterminers = map[string]bool{
	"the": true, "a": true, "an": true,
	"this": true, "that": true, "these": true, "those": true,
	"its": true, "our": true, "their": true, "your": true, "my": true,
	"his": true, "her": true,
	"no": true, "any": true, "some": true, "each": true, "every": true,
	"another": true,
}

// guardImportCues precede a dependency name in the conventional lowercase form
// ("import kafka", "npm install kafka"), so an occurrence after one of them is
// a code position and the proper-name capitalization is not required.
var guardImportCues = map[string]bool{
	"import": true, "require": true, "from": true, "install": true,
	"dependency": true, "package": true,
}

// isIdentRune reports whether r can sit inside a single programming identifier —
// the alphabet the POSITION test uses to decide "fragment of a longer name".
func isIdentRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// runeAt returns the rune at byte offset i in s, or 0 when out of range. Byte
// indexing is sound here: every rune the callers inspect is compared against
// ASCII punctuation or classified by unicode.IsLetter, and a multi-byte rune's
// leading byte is never an ASCII value.
func runeAt(s string, i int) rune {
	if i < 0 || i >= len(s) {
		return 0
	}
	return rune(s[i])
}

// precedingWord returns the run of letters immediately before offset i,
// lowercased, skipping one span of spaces/tabs. "" when the previous non-space
// character is not a letter (punctuation, a digit, a line start) — which is why
// a markdown heading ("## Record") is never read as determiner-governed.
func precedingWord(s string, i int) string {
	j := i
	for j > 0 && (s[j-1] == ' ' || s[j-1] == '\t') {
		j--
	}
	end := j
	for j > 0 && unicode.IsLetter(rune(s[j-1])) {
		j--
	}
	if j == end {
		return ""
	}
	return strings.ToLower(s[j:end])
}

// codePosition reports whether the occurrence of tok at [i, i+len(tok)) sits in
// a code position, where a proper name is conventionally written lowercase and
// the CASE gate must therefore stand down.
//
// The test is whether the whitespace-delimited run around the occurrence
// carries a PATH OR PACKAGE separator — `.`, `/` or `\` (kafka.go,
// kafka-adr-0140.go, kafka.NewProducer(), /record?repo=). Those are the forms
// where a product name is conventionally lowercased. A dependency cue
// ("import kafka") admits the one code form shaped like prose.
//
// Deliberately NOT waived by `_` alone: a snake_case identifier's leading word
// is ordinarily a verb or a qualifier, not a brand — `record_decision` (lema's
// own MCP tool) and `jobs_one_active_knowledge_audit` are exactly the
// pre-existing names this fix exists to stop firing on. Widening the waiver to
// any punctuation re-fires both, which is how this bound was measured rather
// than guessed.
//
// Route and filename forms are deliberately admitted even though that re-opens
// the ambiguity between a NEW `/record` route and one already in the file — a
// lowercase route IS a user-facing surface name. Separating new from
// pre-existing is the novelty gate's job (guardNovelHits), not this one's.
func codePosition(text, tok string, i int) bool {
	start, end := i, i+len(tok)
	for start > 0 && !isSpace(text[start-1]) {
		start--
	}
	for end < len(text) && !isSpace(text[end]) {
		end++
	}
	if strings.ContainsAny(text[start:end], "./\\") {
		return true
	}
	return guardImportCues[precedingWord(text, i)]
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// trailingElement reports whether the occurrence is the LAST element of a
// longer compound identifier — `PublicRecord`, `GetRepoRecord`,
// `public_record`. There the key is the head noun being qualified by something
// else: a PublicRecord is a kind of Record, so the identifier USES the word
// rather than proposing it as a name.
//
// A non-final position is the opposite: in `NewKafkaClient` or `KafkaBrokers`
// the killed name MODIFIES the thing being declared, which is a brand being
// invoked — the guard's best case, pinned by TestOptionMatches and protected by
// d_194bff's rejection of suppressing one-word keys.
func trailingElement(text, tok string, i int) bool {
	return isIdentRune(runeAt(text, i-1)) && !isIdentRune(runeAt(text, i+len(tok)))
}

// namesOption reports whether raw text NAMES the single-word option key —
// the POSITION/CASE/DETERMINER gate described in this file's doc comment. It
// scans the raw text rather than the token set precisely because the token set
// is what erased the distinction: camelCase splitting turns `PublicRecord` into
// a bare `record` indistinguishable from the word itself.
func namesOption(key, text string) bool {
	tok := alnumLower(key)
	if tok == "" {
		return false
	}
	properName := false
	for _, r := range key {
		if unicode.IsLetter(r) {
			properName = unicode.IsUpper(r)
			break
		}
	}
	lower := strings.ToLower(text)
	// Every occurrence is classified, not just the first: one qualifying
	// mention anywhere in the edit is a proposal.
	for off := 0; ; {
		rel := strings.Index(lower[off:], tok)
		if rel < 0 {
			return false
		}
		i := off + rel
		off = i + 1
		if namingOccurrence(text, tok, i, properName) {
			return true
		}
	}
}

// namingOccurrence classifies ONE occurrence of tok at byte offset i.
func namingOccurrence(text, tok string, i int, properName bool) bool {
	// 1. POSITION: as the trailing element of a longer identifier the key is
	// the head noun someone else's name qualifies (PublicRecord), not a name
	// being proposed.
	if trailingElement(text, tok, i) {
		return false
	}
	// 2. CASE: a capitalized key is a proper name and must be capitalized here,
	// unless this is a code position where lowercase is the convention.
	if properName && !unicode.IsUpper(runeAt(text, i)) && !codePosition(text, tok, i) {
		return false
	}
	// 3. DETERMINER: "the Record" uses a noun phrase; "use Record" mentions the
	// name.
	return !guardDeterminers[precedingWord(text, i)]
}

// singleTokenKey returns the lone token of key and true when key is
// single-token — the class this gate governs. A multi-word key is matched by
// contiguous run and is not exposed to the ordinary-word failure.
func singleTokenKey(key string) (string, bool) {
	pieces := verdict.Tokenize(key)
	if len(pieces) != 1 {
		return "", false
	}
	return pieces[0], true
}

// guardNovelHits drops hits whose single-word option ALREADY appears in the
// file's pre-edit content — the second gate, and the only one that can answer
// "did this edit introduce the name?".
//
// PreToolUse runs before the write lands, so the file on disk IS the baseline;
// no git call is needed. A term already in the file is not something this edit
// is proposing — which is what makes `/record?repo=` in a doc comment of
// public_record.go silent while a genuinely new `/record` route in a file that
// never said "record" still fires.
//
// Scoped to single-token keys on purpose (d_23bf88's blast-radius discipline):
// applying novelty to every key would silence a real re-proposal in a document
// that merely discusses the option elsewhere. Fail-open in both directions — an
// unreadable or absent file (a Write creating it) yields no baseline, so
// everything stays a hit.
// guardBaseline reads the pre-edit content of the file a tool call targets.
// PreToolUse runs before the write lands, so this is the file as it stood
// BEFORE the edit. "" on any error — a Write creating a new file, an unreadable
// path, or no file_path at all — which leaves every hit intact (fail-open, the
// guard's standing contract). Bounded so a huge file cannot slow the per-edit
// path; a name absent from the first 256 KiB of a file is novel enough.
func guardBaseline(in map[string]any) string {
	path, _ := in["file_path"].(string)
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 256<<10)
	n, err := f.Read(buf)
	if n <= 0 || (err != nil && err != io.EOF) {
		return ""
	}
	return string(buf[:n])
}

func guardNovelHits(hits []source.Atom, baseline string) []source.Atom {
	if baseline == "" || len(hits) == 0 {
		return hits
	}
	base := tokenSet(baseline)
	out := hits[:0:0]
	for _, h := range hits {
		if tok, ok := singleTokenKey(h.MatchKey); ok && base[tok] {
			continue
		}
		out = append(out, h)
	}
	return out
}
