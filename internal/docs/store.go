package docs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
)

// sweepEvery is the lazy-invalidation debounce: public reads re-stat the scope
// at most this often, so an edited doc is at most a few seconds stale without
// a file watcher, and a poll-heavy UI causes no fs churn.
const sweepEvery = 5 * time.Second

type entry struct {
	doc    Doc
	body   string
	chunks []Chunk
	mtime  time.Time
	size   int64
}

// Store holds the chunked project docs in memory — the docs-side sibling of
// source.Local (ADR-0055). Scan is the full startup pass; sweep is the lazy
// re-scan. All public methods are safe for concurrent use (HTTP handlers).
type Store struct {
	root   string // repo root the engine serves (its CWD)
	adrDir string // discovered ADR dir, slash-separated ("" when none) — group classification
	cfg    Config

	mu        sync.Mutex
	entries   map[string]*entry
	lastSweep time.Time
	now       func() time.Time // injectable for the sweep tests
}

// NewStore builds an empty store; call Scan before serving.
func NewStore(root, adrDir string) *Store {
	return &Store{
		root:    root,
		adrDir:  strings.Trim(filepath.ToSlash(adrDir), "/"),
		cfg:     loadConfig(root),
		entries: map[string]*entry{},
		now:     time.Now,
	}
}

// Scan walks the scope and (re)builds the whole store. Returns the doc count.
func (s *Store) Scan() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rescanLocked()
	return len(s.entries), nil
}

// rescanLocked re-stats every in-scope file: new files load, changed mtime/size
// re-chunk, missing files drop. Unreadable files are skipped loudly — silent
// drops would make the listing lie about what is searchable.
func (s *Store) rescanLocked() {
	live := map[string]bool{}
	for _, rel := range scanFiles(s.root, s.cfg) {
		live[rel] = true
		info, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		if e, ok := s.entries[rel]; ok && e.mtime.Equal(info.ModTime()) && e.size == info.Size() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(rel)))
		if err != nil {
			fmt.Fprintf(os.Stderr, "lema-mcp: docs: skip %s: %v\n", rel, err)
			delete(s.entries, rel)
			continue
		}
		group := s.groupOf(rel)
		title, headings, chunks := chunkBody(rel, group, string(raw))
		if title == "" {
			title = humanize(rel)
		}
		s.entries[rel] = &entry{
			doc:    Doc{Path: rel, Title: title, Group: group, Headings: headings},
			body:   string(raw),
			chunks: chunks,
			mtime:  info.ModTime(),
			size:   info.Size(),
		}
	}
	for rel := range s.entries {
		if !live[rel] {
			delete(s.entries, rel)
		}
	}
	s.lastSweep = s.now()
}

// sweepLocked runs the debounced lazy re-scan.
func (s *Store) sweepLocked() {
	if s.now().Sub(s.lastSweep) < sweepEvery {
		return
	}
	s.rescanLocked()
}

// groupOf classifies a path so the UI can show decisions/openspec once (rich)
// without double-listing their files as plain docs: adr | openspec | spec | doc.
func (s *Store) groupOf(rel string) string {
	switch {
	case s.adrDir != "" && strings.HasPrefix(rel, s.adrDir+"/"):
		return "adr"
	case rel == "openspec" || strings.HasPrefix(rel, "openspec/"):
		return "openspec"
	case strings.HasPrefix(rel, "docs/superpowers/specs/"):
		return "spec"
	default:
		return "doc"
	}
}

// humanize turns a path into a fallback title for heading-less docs.
func humanize(rel string) string {
	b := strings.TrimSuffix(filepath.Base(filepath.FromSlash(rel)), ".md")
	return strings.ReplaceAll(strings.ReplaceAll(b, "-", " "), "_", " ")
}

// List returns every indexed doc, sorted by path.
func (s *Store) List() []Doc {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	out := make([]Doc, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.doc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Get returns one doc and its full body. The map lookup IS the path-traversal
// guard: content is served from memory keyed by the scanned path set — a
// request-supplied path never touches the filesystem.
func (s *Store) Get(path string) (Doc, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e, ok := s.entries[path]
	if !ok {
		return Doc{}, "", false
	}
	return e.doc, e.body, true
}

// Section returns the named section AND its children (trail containment),
// case-insensitive, with headings re-synthesized from trail depth so the
// returned markdown reads as a coherent excerpt.
func (s *Store) Section(path, heading string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	e, ok := s.entries[path]
	if !ok {
		return "", false
	}
	var parts []string
	for _, c := range e.chunks {
		hit := false
		for _, h := range c.Trail {
			if strings.EqualFold(h, heading) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if n := len(c.Trail); n > 0 {
			depth := min(n, 6)
			parts = append(parts, strings.Repeat("#", depth)+" "+c.Trail[n-1])
		}
		parts = append(parts, c.Text)
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, "\n\n"), true
}

// Search scores every chunk with the same physics as the decision search
// (source's scorer, ADR-0055): term hits in the fence-stripped body, weighted
// trail and title hits, density re-rank. Returns the top k as snippet hits.
func (s *Store) Search(query string, k int) []Hit {
	if k <= 0 || k > 50 {
		k = 8
	}
	terms := source.QueryTerms(query)
	if len(terms) == 0 {
		return []Hit{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	type cand struct {
		hit   Hit
		score float64
	}
	var cands []cand
	seen := map[string]bool{}
	for _, e := range s.entries {
		titleL := strings.ToLower(e.doc.Title)
		var titleScore float64
		for _, t := range terms {
			titleScore += 3 * float64(strings.Count(titleL, t))
		}
		for _, c := range e.chunks {
			clean := source.CleanMarkdown(stripFences(c.Text))
			if clean == "" {
				continue
			}
			trailL := strings.ToLower(strings.Join(c.Trail, " "))
			cl := strings.ToLower(clean)
			var hits, trailHits float64
			for _, t := range terms {
				hits += float64(strings.Count(cl, t))
				trailHits += float64(strings.Count(trailL, t))
			}
			if hits == 0 && trailHits == 0 {
				continue
			}
			text := source.BestSnippet(clean, terms, 240)
			norm := strings.ToLower(strings.Join(strings.Fields(text), " "))
			if norm == "" || seen[norm] {
				continue
			}
			seen[norm] = true
			cands = append(cands, cand{
				hit:   Hit{ID: c.ID, Path: c.Path, Group: c.Group, Trail: c.Trail, Text: text},
				score: source.Rerank(3*hits+2*trailHits+titleScore, len([]rune(clean)), "accepted"),
			})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	out := []Hit{}
	for i := 0; i < len(cands) && i < k; i++ {
		h := cands[i].hit
		h.Score = cands[i].score
		out = append(out, h)
	}
	return out
}

// stripFences removes fenced code blocks before scoring — prose carries the
// meaning; a schema dump matching a term would rank noise (same reasoning as
// the decision search skipping fences).
func stripFences(s string) string {
	var out []string
	inFence := false
	for line := range strings.SplitSeq(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
