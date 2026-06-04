// Package docs scans a repo's project markdown (docs/**, openspec/**, root
// README/CLAUDE/AGENTS files, plus .lema/config.json additions), chunks each
// file by heading sections, and serves the chunks from memory — the retrieval
// layer behind search_docs / get_doc and the workbench Docs tab (ADR-0055).
// Like source.Local it is DB-less and seam-clean (ADR-0033): no proprietary
// imports, so it extracts into the public lema-mcp module.
package docs

// Doc is the listing projection for one indexed file.
type Doc struct {
	Path     string   `json:"path"`
	Title    string   `json:"title"`
	Group    string   `json:"group"` // adr | openspec | spec | doc — derived from path
	Headings []string `json:"headings,omitempty"`
}

// Chunk is one heading-bounded section of a doc — the retrieval unit.
type Chunk struct {
	ID    string   `json:"id"`
	Path  string   `json:"path"`
	Group string   `json:"group"`
	Trail []string `json:"trail,omitempty"` // heading ancestry, outermost first
	Text  string   `json:"text"`
}

// Hit is a search result: a chunk with a query-centered snippet as Text.
type Hit struct {
	ID    string   `json:"id"`
	Path  string   `json:"path"`
	Group string   `json:"group"`
	Trail []string `json:"trail,omitempty"`
	Text  string   `json:"text"`
	Score float64  `json:"-"`
}
