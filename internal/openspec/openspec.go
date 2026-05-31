// Package openspec parses an OpenSpec (github.com/Fission-AI/OpenSpec) directory
// — specs/ (the current source of truth) and changes/<id>/ (proposal.md + the
// design.md rationale) — into adr.ADR records, so the same DecisionSource and
// the four MCP tools serve OpenSpec exactly as they serve ADRs. Static parse:
// no network, no model. The proposal carries the why + what; the design carries
// the rationale and the alternatives — OpenSpec's richest why-not signal.
package openspec

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lemahq/lema-mcp/internal/adr"
)

// ParseDir walks an OpenSpec root (the directory that contains specs/ and/or
// changes/) and returns records numbered from startNum upward, in a stable
// ref-sorted order. specs become accepted "current truth" records; each change
// becomes one record whose body is its proposal followed by its design. A
// missing specs/ or changes/ subdir is skipped, not an error.
func ParseDir(root string, startNum int) ([]adr.ADR, error) {
	var recs []adr.ADR

	// specs/<capability>/spec.md — the standing rules.
	specsRoot := filepath.Join(root, "specs")
	specs, err := findByName(specsRoot, "spec.md")
	if err != nil {
		return nil, err
	}
	for _, f := range specs {
		name := relDirName(specsRoot, f)
		body, rerr := os.ReadFile(f)
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", f, rerr)
		}
		recs = append(recs, adr.ADR{
			Slug:   "spec-" + slugify(name),
			Ref:    "openspec/spec/" + name,
			Path:   f,
			Title:  "Spec: " + name,
			Status: "accepted",
			Tags:   []string{"openspec", "spec"},
			Body:   strings.TrimSpace(string(body)),
		})
	}

	// changes/<id>/proposal.md (+ optional design.md) — the why and the why-not.
	changesRoot := filepath.Join(root, "changes")
	proposals, err := findByName(changesRoot, "proposal.md")
	if err != nil {
		return nil, err
	}
	for _, p := range proposals {
		dir := filepath.Dir(p)
		rel := relDirName(changesRoot, p) // "add-auth" or "archive/add-auth"
		archived := rel == "archive" || strings.HasPrefix(rel, "archive/")
		id := strings.TrimPrefix(rel, "archive/")

		proposal, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil, fmt.Errorf("read %s: %w", p, rerr)
		}
		var b strings.Builder
		b.WriteString(strings.TrimSpace(string(proposal)))
		if design, derr := os.ReadFile(filepath.Join(dir, "design.md")); derr == nil {
			b.WriteString("\n\n## Design\n\n")
			b.WriteString(stripFirstH1(strings.TrimSpace(string(design))))
		}
		status := "proposed"
		if archived {
			status = "accepted"
		}
		recs = append(recs, adr.ADR{
			Slug:   "change-" + slugify(id),
			Ref:    "openspec/change/" + id,
			Path:   p,
			Title:  "Change: " + humanize(id),
			Status: status,
			Tags:   []string{"openspec", "change"},
			Body:   b.String(),
		})
	}

	sort.Slice(recs, func(i, j int) bool { return recs[i].Ref < recs[j].Ref })
	for i := range recs {
		recs[i].Number = startNum + i
	}
	return recs, nil
}

// findByName walks dir and returns the paths of every file whose base name is
// exactly name, sorted. A missing dir returns (nil, nil) — the source is optional.
func findByName(dir, name string) ([]string, error) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	var out []string
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == name {
			out = append(out, path)
		}
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, walkErr)
	}
	sort.Strings(out)
	return out, nil
}

// relDirName returns the slash-separated directory of file relative to base
// (base/auth/spec.md -> "auth"; base/a/b/proposal.md -> "a/b").
func relDirName(base, file string) string {
	rel, err := filepath.Rel(base, filepath.Dir(file))
	if err != nil || rel == "." {
		return filepath.Base(filepath.Dir(file))
	}
	return filepath.ToSlash(rel)
}

// stripFirstH1 drops a leading "# Title" line so a design.md folded under a
// "## Design" heading doesn't introduce a competing H1.
func stripFirstH1(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "# ") {
			return strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
		}
		break
	}
	return s
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func humanize(id string) string {
	s := strings.TrimSpace(strings.NewReplacer("-", " ", "_", " ", "/", " · ").Replace(id))
	if s == "" {
		return id
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
