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
	Body         string
}

type frontmatter struct {
	Title        string   `yaml:"title"`
	Status       string   `yaml:"status"`
	Date         string   `yaml:"date"`
	Authors      []string `yaml:"authors"`
	Tags         []string `yaml:"tags"`
	Supersedes   []string `yaml:"supersedes"`
	SupersededBy flexRef  `yaml:"superseded_by"`
	DependsOn    []string `yaml:"depends_on"`
	RelatedTo    []string `yaml:"related_to"`
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
	return a, nil
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
