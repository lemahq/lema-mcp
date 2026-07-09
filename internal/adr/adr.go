// Package adr parses lema-format ADR markdown files (docs/adr/NNNN-slug.md)
// into structured records. It is the Go port of the parsing logic in the
// TypeScript @lema/format package, used by the local-parse DecisionSource so
// the MCP spike can index a repo's existing decisions with zero new behavior
// from the engineer.
package adr

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// ADR is one parsed decision record. Number and Slug come from the filename;
// everything else from the YAML frontmatter, with Body as the markdown after it.
type ADR struct {
	Number       int
	Slug         string
	Ref          string // display ref (e.g. "openspec/change/add-auth"); empty falls back to "ADR-NNNN"
	Path         string
	Title        string
	Status       string
	Date         string
	Authors      []string
	Tags         []string
	Supersedes   []int
	SupersededBy *int
	DependsOn    []int
	RelatedTo    []int
	// Project is the optional project slug (ADR-0061 §8 Phase 1b): resolved to
	// a project_id at ingest, skip-on-miss; empty when absent.
	Project string
	Body    string
}

type frontmatter struct {
	Title        string     `yaml:"title"`
	Status       string     `yaml:"status"`
	Date         string     `yaml:"date"`
	Authors      []string   `yaml:"authors"`
	Tags         []string   `yaml:"tags"`
	Supersedes   []string   `yaml:"supersedes"`
	SupersededBy flexRef    `yaml:"superseded_by"`
	DependsOn    []string   `yaml:"depends_on"`
	RelatedTo    []string   `yaml:"related_to"`
	Project      flexString `yaml:"project"`
}

// flexRef accepts `superseded_by` written as a scalar (`superseded_by: 9` or
// `superseded_by: 0009`), as null, OR as a single-element sequence
// (`superseded_by: [2]`). The canonical form is a scalar — `superseded_by` is
// at most one successor — but the list form is a natural authoring mistake (the
// sibling supersedes/depends_on/related_to fields ARE lists), and some of our
// own dogfood ADRs use it. Rejecting the whole file on that asymmetry would
// crash ParseDir for the entire directory and block the hosted import for any
// repo with one such file, so the parser is lenient: a sequence resolves to its
// first element. The value is captured as a string (not an int) for the same
// octal reason as toNums — strconv.Atoi later forces base-10.
type flexRef struct {
	value *string // nil when absent or null
}

func (f *flexRef) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		// `null`/`~`/empty tag → leave nil; otherwise take the scalar text.
		if node.Tag == "!!null" || node.Value == "" {
			return nil
		}
		v := node.Value
		f.value = &v
		return nil
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			return nil
		}
		// First element only; a multi-successor list is malformed but we take
		// the first rather than failing the parse.
		v := node.Content[0].Value
		f.value = &v
		return nil
	default:
		return fmt.Errorf("superseded_by: unsupported YAML node kind %v", node.Kind)
	}
}

// flexString accepts an optional string field (`project:`) written as a scalar
// (the canonical form), as null, OR as a single-element sequence
// (`project: [payments-replatform]`) — a natural authoring mistake, since the
// sibling tags/supersedes fields ARE lists. The field is optional with
// skip-on-miss semantics (ADR-0061 §8 Phase 1b), so unlike flexRef it never
// returns an error from any node kind: a typed `Project string` would make
// yaml.Unmarshal fail the whole file on the list form, which crashes ParseDir
// for the entire directory and 502s the hosted import for any repo with one
// such file — the exact hazard flexRef exists to prevent for ref fields. A
// malformed value (mapping, empty list, null) degrades to "" — a miss, never
// an error.
type flexString struct {
	value string // "" when absent, null, or an unsupported shape
}

func (f *flexString) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" {
			return nil
		}
		f.value = node.Value
	case yaml.SequenceNode:
		// First scalar element only; a multi-element list is malformed but the
		// first entry is what the author meant.
		if len(node.Content) > 0 && node.Content[0].Kind == yaml.ScalarNode && node.Content[0].Tag != "!!null" {
			f.value = node.Content[0].Value
		}
	}
	// Mapping/alias/anything else: leave "" — a miss, never an error.
	return nil
}

// fileRe matches the canonical docs/adr/NNNN-anything.md naming. Files that
// don't match (README.md, template.md) are skipped by ParseDir.
var fileRe = regexp.MustCompile(`^(\d{4})-(.+)\.md$`)

// firstDigits extracts the leading number from a non-canonical ADR filename. It
// is used only when ParseDirMatching is given a looser pattern (the local wedge
// indexing repos with other ADR naming); canonical NNNN-*.md files take their
// number from fileRe and never reach it.
var firstDigits = regexp.MustCompile(`\d+`)

// utf8BOM is the UTF-8 byte-order-mark some editors prepend; stripped before parsing.
var utf8BOM = string([]byte{0xEF, 0xBB, 0xBF})

// toNums converts frontmatter reference lists to ADR numbers. References are
// parsed as strings (not ints) on purpose: YAML resolves a zero-padded value
// like `0012` as OCTAL, which would silently turn ADR-0012 into 10. strconv.Atoi
// forces base-10, so `0012` -> 12.
func toNums(ss []string) []int {
	var out []int
	for _, s := range ss {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func toNum(s *string) *int {
	if s == nil {
		return nil
	}
	if n, err := strconv.Atoi(strings.TrimSpace(*s)); err == nil {
		return &n
	}
	return nil
}

// ParseDir parses every canonical NNNN-*.md file in dir, sorted by ADR number.
func ParseDir(dir string) ([]ADR, error) {
	return ParseDirMatching(dir, fileRe)
}

// ParseDirMatching parses every file in dir whose basename matches `match`,
// sorted by ADR number. ParseDir uses the canonical NNNN-*.md matcher; the
// local-first MCP passes a looser pattern to index repos that store ADRs under
// other naming conventions, while the hosted curated path stays strict.
func ParseDirMatching(dir string, match *regexp.Regexp) ([]ADR, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read adr dir %s: %w", dir, err)
	}
	var out []ADR
	for _, e := range entries {
		if e.IsDir() || !match.MatchString(e.Name()) {
			continue
		}
		a, err := ParseFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// ParseFile parses a single ADR markdown file from disk.
func ParseFile(path string) (ADR, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ADR{}, fmt.Errorf("read %s: %w", path, err)
	}
	return ParseBytes(raw, filepath.Base(path), path)
}

// ParseBytes parses ADR markdown that came from somewhere other than the
// local filesystem (e.g. fetched from GitHub via the installation client).
// filename supplies the NNNN-slug.md pattern that drives Number+Slug;
// sourcePath is the value stored on ADR.Path for traceability and can be a
// repo-relative path, a github URL, or an empty string.
func ParseBytes(raw []byte, filename, sourcePath string) (ADR, error) {
	fmText, body := splitFrontmatter(string(raw))
	var fm frontmatter
	if fmText != "" {
		if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
			return ADR{}, fmt.Errorf("parse frontmatter %s: %w", filename, err)
		}
	}

	a := ADR{
		Path:         sourcePath,
		Title:        strings.TrimSpace(fm.Title),
		Status:       strings.TrimSpace(fm.Status),
		Date:         strings.TrimSpace(fm.Date),
		Authors:      fm.Authors,
		Tags:         fm.Tags,
		Supersedes:   toNums(fm.Supersedes),
		SupersededBy: toNum(fm.SupersededBy.value),
		DependsOn:    toNums(fm.DependsOn),
		RelatedTo:    toNums(fm.RelatedTo),
		Project:      strings.TrimSpace(fm.Project.value),
		Body:         strings.TrimSpace(body),
	}

	if m := fileRe.FindStringSubmatch(filename); m != nil {
		a.Number, _ = strconv.Atoi(m[1])
		a.Slug = m[2]
	} else {
		// Non-canonical filename (reached via ParseDirMatching with a looser
		// pattern): take the first digit-run as the number and the extension-less
		// name as the slug. Canonical NNNN-*.md files never reach this branch, so
		// the strict hosted/import path is unchanged.
		a.Slug = strings.TrimSuffix(filename, ".md")
		if d := firstDigits.FindString(filename); d != "" {
			a.Number, _ = strconv.Atoi(d)
		}
	}
	if a.Title == "" {
		a.Title = firstH1(body)
	}
	if a.Status == "" {
		a.Status = statusFromBody(body)
	}
	return a, nil
}

// statusBulletRe matches MADR 2.x's metadata bullet (`- Status: accepted`,
// `* Status: REJECTED`), valid only in the metadata block between the H1 and
// the first section heading — a "Status:" line inside later prose is not a
// status marker.
var statusBulletRe = regexp.MustCompile(`(?i)^\s*[-*]\s*\**status\**\s*:\s*(.+)$`)

// statusHeadingRe matches a Nygard/adr-tools `## Status` section heading (any
// level); the status text is the first non-empty line of the section.
var statusHeadingRe = regexp.MustCompile(`(?i)^\s*#{1,6}\s+status\s*$`)

// sectionHeadingRe marks the end of the MADR metadata block (any level-2+
// heading).
var sectionHeadingRe = regexp.MustCompile(`^\s*#{2,6}\s`)

// statusKeywordRe extracts canonical status words from a sniffed status text.
var statusKeywordRe = regexp.MustCompile(`(?i)\b(proposed|accepted|superseded|deprecated|rejected)\b`)

// statusFromBody sniffs a decision status from markdown that carries no
// frontmatter status — the norm for foreign repos (MADR 2.x metadata bullets,
// Nygard `## Status` sections). Without it every imported foreign ADR landed
// as a proposal: 48/49 "in deliberation" on the 2026-06-12 fxa stranger walk.
// Returns a canonical lowercase status word, or "" when no marker is present
// or the marker is ambiguous — callers decide the default (the hosted import
// treats "" as a record in force).
func statusFromBody(body string) string {
	inMetadataBlock := true
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if statusHeadingRe.MatchString(line) {
			for _, next := range lines[i+1:] {
				next = strings.TrimSpace(next)
				if next == "" {
					continue
				}
				if strings.HasPrefix(next, "#") {
					return "" // empty section
				}
				return canonicalStatus(next)
			}
			return ""
		}
		if sectionHeadingRe.MatchString(line) {
			inMetadataBlock = false
			continue
		}
		if inMetadataBlock {
			if m := statusBulletRe.FindStringSubmatch(line); m != nil {
				return canonicalStatus(m[1])
			}
		}
	}
	return ""
}

// canonicalStatus reduces a sniffed status text to one canonical word.
// "Superseded by [ADR-0017](...)" → "superseded"; exactly one DISTINCT
// keyword is required — a text naming several (like a template's
// "[proposed | rejected | accepted | …]" placeholder) is no signal at all.
func canonicalStatus(text string) string {
	distinct := map[string]bool{}
	first := ""
	for _, m := range statusKeywordRe.FindAllString(text, -1) {
		w := strings.ToLower(m)
		if !distinct[w] {
			distinct[w] = true
			if first == "" {
				first = w
			}
		}
	}
	if len(distinct) == 1 {
		return first
	}
	return ""
}

// splitFrontmatter separates a leading `---`-fenced YAML block from the body.
// The closing fence is the first line that is exactly `---`, so horizontal
// rules later in the body are never mistaken for it.
func splitFrontmatter(s string) (fm, body string) {
	s = strings.TrimPrefix(s, utf8BOM)
	lines := strings.Split(s, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return "", s
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n")
		}
	}
	return "", s // no closing fence — treat whole file as body
}

func firstH1(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(line[2:])
		}
	}
	return ""
}

// genericHeadings are RFC/ADR template section words that must never become a
// decision's display title. The load-bearing case (#361): the react/vue RFC
// templates open with `# Summary` (an H1, unlike rust's `## Summary` H2), so
// ParseBytes' firstH1 puts "Summary" in Title — the string every react/vue
// decision was titled on the public permalinks. The rest are the standard
// RFC-2119/MADR section headers, denied defensively so a template that leads
// with any of them can't leak the section word as the title either. Match is
// exact (normalized), so a real title that merely *starts* with one of these
// words — go's "Proposal: Goroutine leak detection…" — is kept.
var genericHeadings = map[string]bool{
	"summary":                     true,
	"motivation":                  true,
	"abstract":                    true,
	"overview":                    true,
	"background":                  true,
	"introduction":                true,
	"context":                     true,
	"goals":                       true,
	"non-goals":                   true,
	"proposal":                    true,
	"basic example":               true,
	"detailed design":             true,
	"guide-level explanation":     true,
	"reference-level explanation": true,
	"drawbacks":                   true,
	"alternatives":                true,
	"rationale and alternatives":  true,
	"prior art":                   true,
	"unresolved questions":        true,
	"future possibilities":        true,
	"how we teach this":           true,
	"adoption strategy":           true,
}

// isGenericHeading reports whether s is nothing but a template section heading
// (case-insensitive, trailing colon tolerated) and so must not be used as a title.
func isGenericHeading(s string) bool {
	norm := strings.ToLower(strings.TrimSpace(s))
	norm = strings.TrimSpace(strings.TrimRight(norm, ":"))
	return genericHeadings[norm]
}

// titleMetaRe matches a `Title:` metadata line — a dash/star bullet or a bare
// line, with optional bold markers (`- Title:`, `* **Title**:`, `Title:`) — the
// value is the display title. Some RFC templates carry it; react/vue/rust do
// not (they use Start Date / Feature Name / RFC PR), so this is a fallback that
// only fires for formats that provide it.
var titleMetaRe = regexp.MustCompile(`(?i)^\s*[-*]?\s*\**title\**\s*:\s*(.+)$`)

// titleFromMetadata scans the leading metadata block (everything above the first
// markdown heading) for a `Title:` line and returns its value. It stops at the
// first heading so a "Title:" written inside later prose is never mistaken for
// the record's title — the same block-scoping statusFromBody uses.
func titleFromMetadata(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			break // reached the first heading; the metadata block is above it
		}
		if m := titleMetaRe.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	return ""
}

// humanizeSlug turns a filename slug into a fallback title: separators to
// spaces, collapsed whitespace, first letter upper (sentence case, e.g.
// "0068-react-hooks" parsed to slug "react-hooks" → "React hooks"). The
// filename's NNNN-<slug>.md is always descriptive, so the slug is a faithful
// title source when a doc carries no usable heading or metadata title.
func humanizeSlug(slug string) string {
	s := strings.NewReplacer("-", " ", "_", " ").Replace(strings.TrimSpace(slug))
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// DeriveTitle chooses the best display title for a parsed decision record,
// preferring, in order: (a) the parsed Title (a YAML/frontmatter title or the
// document's H1, as set by ParseBytes) when it is not a generic section
// heading; (b) a `Title:` metadata line from the body; (c) the humanized
// filename slug. It NEVER returns a generic section heading (Summary, Motivation,
// Detailed design, …) — that is the #361 bug: flat-rfc react/vue docs open with
// `# Summary`, which ParseBytes' firstH1 would otherwise hand back as the title.
// Returns "" only when there is genuinely nothing to name the record (no usable
// title AND no slug), which callers treat as skip.
//
// This is the single title-derivation used by the public-corpus seeders
// (cmd/lema-sync, cmd/lema-demo-seed). ParseBytes' own firstH1 assignment is
// left unchanged so the hosted-import and local-wedge parse paths are untouched.
func DeriveTitle(a ADR) string {
	if t := strings.TrimSpace(a.Title); t != "" && !isGenericHeading(t) {
		return t
	}
	if t := titleFromMetadata(a.Body); t != "" && !isGenericHeading(t) {
		return t
	}
	return humanizeSlug(a.Slug)
}
