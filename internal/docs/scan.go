package docs

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Config is the optional .lema/config.json docs block — lema's first config
// file (ADR-0055), kept deliberately tiny. Include entries are extra roots
// (a directory walked recursively, or a single file); exclude entries are
// repo-relative path prefixes carved out of the scope. Prefixes, not globs:
// stdlib has no '**' matcher and a glob dependency is not warranted.
type Config struct {
	Docs struct {
		Include []string `json:"include"`
		Exclude []string `json:"exclude"`
	} `json:"docs"`
}

// loadConfig reads .lema/config.json under root. Missing file → zero value.
// Malformed file → zero value with a loud stderr line, never a crash: the
// engine is the workbench's sidecar and a config typo must not brick the app.
func loadConfig(root string) Config {
	b, err := os.ReadFile(filepath.Join(root, ".lema", "config.json"))
	if err != nil {
		return Config{}
	}
	c, err := ParseConfig(b)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lema-mcp: docs: bad .lema/config.json (%v); using default roots\n", err)
		return Config{}
	}
	return c
}

// maxDocBytes caps indexed files at 1 MB — bigger markdown is a generated
// artifact (a changelog dump, a bundled spec), not team docs.
const maxDocBytes = 1 << 20

var (
	defaultDirs  = []string{"docs", "openspec"}
	defaultFiles = []string{"README.md", "CLAUDE.md", "AGENTS.md"}
	// skipDirs are never descended into, under any root: vendored or generated
	// markdown is retrieval noise (the reason ADR-0055 rejected all-repo scope).
	skipDirs = map[string]bool{
		"node_modules": true, ".git": true, "dist": true, "build": true,
		"target": true, "vendor": true, ".next": true,
	}
)

// scanFiles returns the sorted repo-relative (slash-separated) paths of every
// markdown file in scope: default roots + config includes, minus excludes,
// skip-dirs, and files over the size cap.
func scanFiles(root string, cfg Config) []string {
	seen := map[string]bool{}
	addFile := func(rel string) {
		rel = filepath.ToSlash(rel)
		// InScope (scope.go) is the ONE definition of project-doc scope,
		// shared with the server-side scanner. Everything path-shaped lives
		// there; only the size cap stays here, because only this side has a
		// filesystem to stat.
		if seen[rel] || !InScope(rel, cfg) {
			return
		}
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil || info.Size() > MaxDocBytes {
			return
		}
		seen[rel] = true
	}
	walkDir := func(dir string) {
		base := filepath.Join(root, filepath.FromSlash(dir))
		_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // unreadable subtree: skip, don't abort the scan
			}
			if d.IsDir() {
				if SkipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil
			}
			addFile(rel)
			return nil
		})
	}
	for _, d := range defaultDirs {
		walkDir(d)
	}
	for _, f := range defaultFiles {
		addFile(f)
	}
	absRoot, absRootErr := filepath.Abs(root)
	for _, inc := range cfg.Docs.Include {
		inc = strings.Trim(filepath.ToSlash(inc), "/")
		if inc == "" {
			continue
		}
		candidate := filepath.Join(root, filepath.FromSlash(inc))
		absCandidate, absCandErr := filepath.Abs(candidate)
		if absRootErr != nil || absCandErr != nil || !strings.HasPrefix(absCandidate+string(os.PathSeparator), absRoot+string(os.PathSeparator)) {
			fmt.Fprintf(os.Stderr, "lema-mcp: docs: skipping out-of-tree path %q\n", inc)
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			walkDir(inc)
		} else {
			addFile(inc)
		}
	}
	out := make([]string, 0, len(seen))
	for rel := range seen {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}
