package verdict

import (
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// This is the ADR-0053 recall-calibration eval — the labeled set the matcher is
// tuned against, and the precision/recall spec it must hold. The old matcher
// required EVERY token of a (often full-sentence) option name to appear in the
// query, so recall fell to ~zero on natural-language topics; a live dogfood
// check_decided missed a decision recorded 30 seconds earlier. The new matcher
// scores by distinctiveness-weighted overlap, so a query that names the killed
// option's salient terms fires — without generic word-overlap ("store",
// "database") tripping a false never-reopen, which is an uninstall.
//
// The corpus mirrors lema's own closed decisions; the cases are natural-language
// topics an agent would actually type. A positive must surface its expected
// decision; a negative must surface NOTHING. One threshold must separate them —
// if it can't, the matcher design is wrong, which is exactly what eval-first
// exists to catch.

func evalCorpus() []source.Atom {
	mk := func(key string) source.Atom {
		return source.Atom{Closed: true, MatchKey: key, Ref: key, Text: key}
	}
	return []source.Atom{
		mk("MongoDB Atlas Vector Search"),
		mk("Pinecone / dedicated vector database"),
		mk("Embed code or document chunks for retrieval (chunk-RAG)"),
		mk("Store source bodies alongside the claims"),
		mk("A polyrepo approach"),
		mk("Nx or Turborepo for monorepo tooling"),
		mk("A TypeScript-only backend"),
	}
}

func TestGuardMatchRecall(t *testing.T) {
	corpus := evalCorpus()

	positives := []struct {
		topic   string
		wantKey string // the MatchKey the top hit must carry
	}{
		{"should we use MongoDB for the vector store?", "MongoDB Atlas Vector Search"},
		{"let's switch the index to Pinecone", "Pinecone / dedicated vector database"},
		// The live dogfood miss: names the option's salient terms without the
		// full phrase ("document", "chunk-RAG" absent).
		{"store source bodies and embed code chunks for retrieval", "Embed code or document chunks for retrieval (chunk-RAG)"},
		{"I think we should go with a polyrepo", "A polyrepo approach"},
		{"set up Turborepo for the monorepo", "Nx or Turborepo for monorepo tooling"},
		{"write the backend in TypeScript only", "A TypeScript-only backend"},
	}
	for _, tt := range positives {
		t.Run("hit/"+tt.wantKey, func(t *testing.T) {
			hits := Match(corpus, tt.topic, MatchThreshold)
			if len(hits) == 0 {
				t.Fatalf("topic %q matched NOTHING, want top hit %q (recall miss)", tt.topic, tt.wantKey)
			}
			if hits[0].MatchKey != tt.wantKey {
				t.Errorf("topic %q top hit = %q, want %q", tt.topic, hits[0].MatchKey, tt.wantKey)
			}
		})
	}
}

func TestGuardMatchPrecision(t *testing.T) {
	corpus := evalCorpus()

	// Negatives: topics that share only generic words with a killed option, or
	// nothing at all. A false never-reopen here is an uninstall, so these MUST
	// return empty.
	negatives := []string{
		"store the user config in the database",     // "store"/"database" are generic, not the option
		"add request logging to the API middleware", // shares nothing distinctive
		"rename a variable in the parser",           // unrelated
		"improve CI pipeline speed",                 // unrelated
		"add a retry to the vector embedding call",  // "vector"/"embedding" generic, not "MongoDB"/"Pinecone"
	}
	for _, topic := range negatives {
		t.Run("miss/"+topic, func(t *testing.T) {
			hits := Match(corpus, topic, MatchThreshold)
			if len(hits) != 0 {
				t.Errorf("topic %q FALSE-matched %q (false never-reopen = uninstall)", topic, hits[0].MatchKey)
			}
		})
	}
}

// significantTokens drops function/generic words so the match rests on the
// option's distinctive terms, not "use"/"the"/"approach".
func TestSignificantTokens(t *testing.T) {
	got := significantTokens("Embed code or document chunks for retrieval (chunk-RAG)")
	set := map[string]bool{}
	for _, tok := range got {
		set[tok] = true
	}
	for _, want := range []string{"embed", "code", "document", "chunks", "retrieval", "rag"} {
		if !set[want] {
			t.Errorf("significantTokens dropped distinctive token %q: %v", want, got)
		}
	}
	for _, stop := range []string{"or", "for", "the", "a", "an"} {
		if set[stop] {
			t.Errorf("significantTokens kept stopword %q: %v", stop, got)
		}
	}
}

// Prod false positive, 2026-07-08 (the d_20d515 dogfood FP): a topic about
// building a rest-state conflicts scan false-fired ruled_out against the
// killed option "Also condense the authed tool surface now" purely on the
// generic nouns "authed"+"surface". Those are surface-area vocabulary, never
// an option's identity — the identity of that ruling is "condense".
func TestGuardMatchGenericSurfaceNouns(t *testing.T) {
	// Real-shaped corpus: IDF needs neighbors to make a rare term distinctive
	// (a 1-atom corpus caps any single-token weight below MatchThreshold).
	corpus := append(evalCorpus(), source.Atom{
		ID:       "d_20d515-rej-0",
		MatchKey: "Also condense the authed tool surface now",
		Type:     "rejected_alternative",
	})

	fp := "Build a rest-state conflict scan over the recorded decisions plus an internal authed web surface that lists contradictions, duplicate captures, and pending amendments between decisions (the Precedent Scan / conflicts view)"
	if hits := Match(corpus, fp, MatchThreshold); len(hits) != 0 {
		t.Errorf("generic-noun overlap false-fired: %q matched %q", fp, hits[0].MatchKey)
	}

	// Recall guard: actually re-proposing the killed option must still fire —
	// its identity survives on "condense".
	relit := "let's condense the authed tool surface in this release"
	if hits := Match(corpus, relit, MatchThreshold); len(hits) != 1 {
		t.Errorf("true relitigation of the condense ruling must still match, got %d hits", len(hits))
	}

	// Tokenize doesn't stem — the plural forms must be stopworded too, or the
	// same FP class re-fires on "surfaces"/"tools" phrasing.
	fpPlural := "an internal authed web surfaces page listing the agent tools and pending amendments"
	if hits := Match(corpus, fpPlural, MatchThreshold); len(hits) != 0 {
		t.Errorf("plural generic-noun overlap false-fired: matched %q", hits[0].MatchKey)
	}
}
