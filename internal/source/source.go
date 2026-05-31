// Package source is the seam between the MCP tools and where decisions come
// from. The spike ships Local (parses docs/adr/ on disk); the hosted tier will
// add an API-backed implementation behind the same DecisionSource interface,
// so the open-source server extracts cleanly without touching the tools.
package source

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/lemahq/lema-mcp/internal/adr"
)

// Summary is the lightweight projection returned by list and search.
type Summary struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Tags   []string `json:"tags,omitempty"`
}

// Edge is one typed relationship from a decision to another decision number.
type Edge struct {
	Type string `json:"type"`
	To   int    `json:"to"`
}

// Detail is the full decision, including body and edges.
type Detail struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Date   string   `json:"date,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	Edges  []Edge   `json:"edges,omitempty"`
	Body   string   `json:"body"`
}

// GraphNode / GraphEdge / Graph describe a connected subgraph of decisions.
type GraphNode struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type GraphEdge struct {
	From int    `json:"from"`
	To   int    `json:"to"`
	Type string `json:"type"`
}

type Graph struct {
	Root  int         `json:"root"`
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// Atom is a retrieved claim in the consumption shape the MCP search tool serves
// (ADR-0025 §4): id, type, text, ref. The DB-less local backend (lexical over
// ADR sections) and the hosted atom-search backend both return these, so
// search_decisions is backend-agnostic and the open-source binary needs no
// pgvector/Vertex dependency.
//
// Edges is the additive edge-aware annotation (2026-05-30 spec): the typed
// decision-graph relationships of the atom's source decision, so an agent that
// searched a claim immediately sees what it supersedes / depends on / ruled out
// without a second get_decision_graph round trip. Locally these come from the
// ADR's frontmatter; the hosted backend fills the same field from the edges
// table. It is attached after ranking and omits when empty, so it never affects
// the order search_decisions returns.
type Atom struct {
	ID    string  `json:"id"`
	Type  string  `json:"type"`
	Text  string  `json:"text"`
	Ref   string  `json:"ref"`
	Edges []Edge  `json:"edges,omitempty"`
	Score float64 `json:"-"`
}

// DecisionSource is the interface the four MCP tools call. Swapping the
// implementation (local-parse vs hosted API) is a one-line composition change.
type DecisionSource interface {
	List(ctx context.Context, status string, limit int) ([]Summary, error)
	Get(ctx context.Context, number int) (Detail, error)
	Search(ctx context.Context, query string, k int) ([]Atom, error)
	Graph(ctx context.Context, number, depth int) (Graph, error)
}

// Local is an in-memory DecisionSource over a parsed docs/adr/ directory.
// Lexical search; graph from frontmatter edges. No database, no embeddings —
// enough to test whether retrieved context changes agent behavior.
type Local struct {
	byNum map[int]adr.ADR
	order []int
}

// NewLocal builds a Local index from parsed ADRs.
func NewLocal(adrs []adr.ADR) *Local {
	l := &Local{byNum: make(map[int]adr.ADR, len(adrs))}
	for _, a := range adrs {
		l.byNum[a.Number] = a
		l.order = append(l.order, a.Number)
	}
	sort.Ints(l.order)
	return l
}

func summaryOf(a adr.ADR) Summary {
	return Summary{Number: a.Number, Title: a.Title, Status: a.Status, Tags: a.Tags}
}

func edgesOf(a adr.ADR) []Edge {
	var e []Edge
	for _, t := range a.Supersedes {
		e = append(e, Edge{Type: "supersedes", To: t})
	}
	if a.SupersededBy != nil {
		e = append(e, Edge{Type: "superseded_by", To: *a.SupersededBy})
	}
	for _, t := range a.DependsOn {
		e = append(e, Edge{Type: "depends_on", To: t})
	}
	for _, t := range a.RelatedTo {
		e = append(e, Edge{Type: "related_to", To: t})
	}
	return e
}

// List returns decisions, optionally filtered by status, sorted by number.
func (l *Local) List(_ context.Context, status string, limit int) ([]Summary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	out := []Summary{}
	for _, n := range l.order {
		a := l.byNum[n]
		if status != "" && !strings.EqualFold(a.Status, status) {
			continue
		}
		out = append(out, summaryOf(a))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Get returns one decision's full detail by ADR number.
func (l *Local) Get(_ context.Context, number int) (Detail, error) {
	a, ok := l.byNum[number]
	if !ok {
		return Detail{}, fmt.Errorf("decision %d not found", number)
	}
	return Detail{
		Number: a.Number, Title: a.Title, Status: a.Status, Date: a.Date,
		Tags: a.Tags, Edges: edgesOf(a), Body: a.Body,
	}, nil
}

// Search returns the atoms most relevant to the query. It splits each ADR into
// sections, then into blocks (paragraphs and list items), scores blocks by query
// term overlap weighted by the ADR title/tags and section heading, and returns a
// tight, query-centered, markdown-cleaned snippet of each matching block — not a
// whole section. This is the DB-less local backend (no embeddings) behind the
// same interface the hosted hybrid dense+lexical atom search fills, so
// search_decisions never changes when the backend is swapped.
func (l *Local) Search(_ context.Context, query string, k int) ([]Atom, error) {
	if k <= 0 || k > 50 {
		k = 8
	}
	terms := queryTerms(query)
	if len(terms) == 0 {
		return []Atom{}, nil
	}
	type cand struct {
		atom  Atom
		score float64
	}
	var cands []cand
	seen := map[string]bool{}
	for _, n := range l.order {
		a := l.byNum[n]
		ref := a.Ref
		if ref == "" {
			ref = fmt.Sprintf("ADR-%04d", a.Number)
		}
		titleL := strings.ToLower(a.Title)
		tagsL := strings.ToLower(strings.Join(a.Tags, " "))
		var titleScore float64
		for _, t := range terms {
			titleScore += 4 * float64(strings.Count(titleL, t))
			titleScore += 2 * float64(strings.Count(tagsL, t))
		}

		var adrCands []cand
		var leadText, leadType string
		var leadID int
		bi := 0
		for _, sec := range splitSections(a.Body) {
			typ := sectionType(sec.heading)
			headL := strings.ToLower(sec.heading)
			var headScore float64
			for _, t := range terms {
				headScore += 2 * float64(strings.Count(headL, t))
			}
			for _, raw := range splitBlocks(sec.text) {
				clean := cleanMarkdown(raw)
				if clean == "" {
					continue
				}
				bi++
				if leadText == "" {
					leadText, leadType, leadID = clean, typ, bi
				}
				var hits float64
				cl := strings.ToLower(clean)
				for _, t := range terms {
					hits += float64(strings.Count(cl, t))
				}
				if hits == 0 {
					continue
				}
				text := bestSnippet(clean, terms, 240)
				norm := strings.ToLower(strings.Join(strings.Fields(text), " "))
				if seen[norm] {
					continue
				}
				seen[norm] = true
				adrCands = append(adrCands, cand{
					atom:  Atom{ID: fmt.Sprintf("%d-%d", a.Number, bi), Type: typ, Text: text, Ref: ref, Edges: edgesOf(a)},
					score: rerank(3*hits+headScore+titleScore, len([]rune(clean)), a.Status),
				})
			}
		}
		switch {
		case len(adrCands) > 0:
			cands = append(cands, adrCands...)
		case titleScore > 0 && leadText != "":
			// Only the title/tags matched — surface one representative lead block so
			// a title hit still returns something, without flooding with every block.
			cands = append(cands, cand{
				atom:  Atom{ID: fmt.Sprintf("%d-%d", a.Number, leadID), Type: leadType, Text: bestSnippet(leadText, terms, 240), Ref: ref, Edges: edgesOf(a)},
				score: rerank(titleScore, len([]rune(leadText)), a.Status),
			})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	out := []Atom{}
	for i := 0; i < len(cands) && i < k; i++ {
		atom := cands[i].atom
		atom.Score = cands[i].score
		out = append(out, atom)
	}
	return out, nil
}

type section struct{ heading, text string }

// splitSections breaks an ADR body into ## / ### sections (heading + text).
// Content before the first heading becomes a preamble section with no heading.
func splitSections(body string) []section {
	var secs []section
	cur := section{}
	flush := func() {
		cur.text = strings.TrimSpace(cur.text)
		if cur.heading != "" || cur.text != "" {
			secs = append(secs, cur)
		}
	}
	for ln := range strings.SplitSeq(body, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "## ") || strings.HasPrefix(t, "### ") {
			flush()
			cur = section{heading: strings.TrimSpace(strings.TrimLeft(t, "# "))}
			continue
		}
		cur.text += ln + "\n"
	}
	flush()
	return secs
}

// sectionType maps a canonical ADR heading to a claim type for the §4 contract.
func sectionType(heading string) string {
	switch strings.ToLower(strings.TrimSpace(heading)) {
	// "end state" is the Commander's Intent win-condition — a chosen target.
	case "decision", "what changes", "what", "design", "approach", "end state", "end-state":
		return "chosen"
	case "alternatives", "alternatives considered", "other approaches", "rejected":
		return "rejected"
	case "consequences", "tradeoffs", "trade-offs", "impact":
		return "consequence"
	// Commander's Intent: purpose / must-be-true / known constraints are all constraints.
	case "context", "why", "purpose", "requirements", "added requirements", "modified requirements",
		"constraints", "must be true", "must-be-true", "known constraints":
		return "constraint"
	default:
		return "decision"
	}
}

// queryTerms lowercases the query, strips punctuation, and drops a small
// stopword set so ranking keys on the meaningful terms ("why did we choose
// postgres" -> choose, postgres) rather than matching "the"/"why" everywhere.
func queryTerms(q string) []string {
	var out []string
	for w := range strings.FieldsSeq(strings.ToLower(q)) {
		w = strings.Trim(w, ".,?!\"'()[]:;\x60")
		if len(w) < 2 || stopwords[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}

var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true, "to": true,
	"in": true, "on": true, "for": true, "is": true, "are": true, "was": true, "were": true,
	"be": true, "we": true, "our": true, "why": true, "how": true, "what": true, "did": true,
	"do": true, "does": true, "this": true, "that": true, "it": true, "with": true, "as": true,
	"by": true, "use": true, "using": true,
}

var (
	mdImage    = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)  // ![alt](url) -> drop
	mdLink     = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`) // [text](url) -> text
	mdListItem = regexp.MustCompile(`^(?:[-*+]|\d+\.)\s+`)   // leading list marker
	wsRun      = regexp.MustCompile(`\s+`)
)

// splitBlocks breaks a section's text into ranking units: paragraphs (blank-line
// separated) and individual list items. Fenced code blocks are skipped — the
// decision's prose carries the "why", not the schema dump.
func splitBlocks(sectionText string) []string {
	var blocks []string
	var para []string
	flush := func() {
		if len(para) > 0 {
			blocks = append(blocks, strings.Join(para, " "))
			para = para[:0]
		}
	}
	inFence := false
	for line := range strings.SplitSeq(sectionText, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			flush()
			inFence = !inFence
			continue
		}
		if inFence || t == "" {
			flush()
			continue
		}
		if loc := mdListItem.FindStringIndex(t); loc != nil {
			flush()
			blocks = append(blocks, t[loc[1]:])
			continue
		}
		para = append(para, t)
	}
	flush()
	return blocks
}

// cleanMarkdown reduces a block to readable plain text: drops images, unwraps
// links to their text, removes backticks, and collapses whitespace. It leaves
// '_' and '*' alone so identifiers like org_id survive intact.
func cleanMarkdown(s string) string {
	s = mdImage.ReplaceAllString(s, "")
	s = mdLink.ReplaceAllString(s, "$1")
	s = strings.ReplaceAll(s, "`", "")
	s = wsRun.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// bestSnippet returns a tight, query-centered window of clean text: the whole
// block when it already fits maxLen runes, else a maxLen-rune window around the
// first query-term hit, trimmed to word boundaries with ellipses.
func bestSnippet(clean string, terms []string, maxLen int) string {
	r := []rune(clean)
	if len(r) <= maxLen {
		return clean
	}
	lower := strings.ToLower(clean)
	hitByte := -1
	for _, t := range terms {
		if i := strings.Index(lower, t); i >= 0 && (hitByte < 0 || i < hitByte) {
			hitByte = i
		}
	}
	hitRune := 0
	if hitByte > 0 {
		hitRune = len([]rune(clean[:hitByte]))
	}
	start := max(hitRune-maxLen/3, 0)
	end := start + maxLen
	if end > len(r) {
		end = len(r)
		start = max(end-maxLen, 0)
	}
	seg := string(r[start:end])
	if start > 0 {
		if i := strings.IndexByte(seg, ' '); i >= 0 {
			seg = seg[i+1:]
		}
	}
	if end < len(r) {
		if i := strings.LastIndexByte(seg, ' '); i >= 0 {
			seg = seg[:i]
		}
	}
	seg = strings.TrimSpace(seg)
	if start > 0 {
		seg = "… " + seg
	}
	if end < len(r) {
		seg = seg + " …"
	}
	return seg
}

// rerank applies the density re-rank from ADR-0025 §7 on top of the raw lexical
// score: prefer tight atoms (length normalization) and current decisions (status
// weight). It is a tunable, not a law — change a weight, re-run cmd/lema-eval,
// and keep it only if hit-rate/MRR/recall improve.
func rerank(base float64, runes int, status string) float64 {
	return base * lengthNorm(runes) * statusWeight(status)
}

// lengthNorm favors tight, dense atoms: a ~120-rune claim scores near 1.0 and
// longer blocks decay by inverse-log, so a long sprawling section cannot out-rank
// a precise claim merely by accumulating term occurrences.
func lengthNorm(runes int) float64 {
	return 1.0 / (1.0 + math.Log1p(float64(runes)/120.0))
}

// statusWeight down-weights decisions that no longer hold, so a superseded or
// rejected ADR ranks below the current one when both match a query.
func statusWeight(status string) float64 {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "accepted":
		return 1.0
	case "superseded", "deprecated", "rejected":
		return 0.4
	default: // proposed / unknown
		return 0.85
	}
}

// Graph does a bounded BFS over typed frontmatter edges from a decision.
// Edges to decisions not present in the index are still reported (the
// reference is real even if the target ADR isn't in this directory).
func (l *Local) Graph(_ context.Context, number, depth int) (Graph, error) {
	if _, ok := l.byNum[number]; !ok {
		return Graph{}, fmt.Errorf("decision %d not found", number)
	}
	if depth <= 0 {
		depth = 1
	}
	if depth > 5 {
		depth = 5
	}
	g := Graph{Root: number, Nodes: []GraphNode{}, Edges: []GraphEdge{}}
	seenNode := map[int]bool{}
	seenEdge := map[string]bool{}
	type item struct{ num, d int }
	queue := []item{{number, 0}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		a, ok := l.byNum[cur.num]
		if ok && !seenNode[cur.num] {
			seenNode[cur.num] = true
			g.Nodes = append(g.Nodes, GraphNode{Number: a.Number, Title: a.Title, Status: a.Status})
		}
		if !ok || cur.d >= depth {
			continue
		}
		for _, e := range edgesOf(a) {
			key := fmt.Sprintf("%d:%s:%d", cur.num, e.Type, e.To)
			if !seenEdge[key] {
				seenEdge[key] = true
				g.Edges = append(g.Edges, GraphEdge{From: cur.num, To: e.To, Type: e.Type})
			}
			queue = append(queue, item{e.To, cur.d + 1})
		}
	}
	return g, nil
}
