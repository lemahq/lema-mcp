package docs

// Project-doc SCOPE POLICY — the single definition of "which files in a repo
// are project docs" (ADR-0055, d_8564d1: known roots plus .lema/config.json
// globs, never an all-repo markdown scan).
//
// It lives here, in the package that already owns the definition, because two
// consumers now need it and two copies would drift — pain-point #19, two
// things in one codebase disagreeing about the current shape (decision
// a827fc9d):
//
//	internal/docs.scanFiles   local-filesystem walker, runs in lema-mcp
//	internal/specscan         Git Trees listing, runs server-side (no checkout)
//
// The walker itself is NOT shareable: the API server has no working copy. So
// everything below is PATH-ONLY and pure — no os.Stat, no WalkDir — and the
// filesystem-dependent half (the size cap) stays with the caller that has a
// filesystem. specscan imports this package for the policy and supplies its
// own listing mechanism.
//
// Seam note: keeping this inside internal/docs is also what keeps
// scripts/extract-lema-mcp.sh working untouched — it copies
// internal/{adr,source,openspec,docs,verdict,decisioncheck}/*.go by name, so
// a new file here ships to the public lema-mcp module automatically, where a
// new shared package would have left it uncompilable.

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// MaxDocBytes caps indexed files at 1 MB — bigger markdown is a generated
// artifact (a changelog dump, a bundled spec), not team docs. Exported for
// the server-side consumer, which applies it to a blob size it learns from
// the API rather than from os.Stat.
const MaxDocBytes = maxDocBytes

// DefaultDirs are the directory roots walked recursively by default.
func DefaultDirs() []string { return append([]string(nil), defaultDirs...) }

// DefaultFiles are the repo-root files in scope by default.
func DefaultFiles() []string { return append([]string(nil), defaultFiles...) }

// SkipDir reports whether a directory NAME is never descended into, under any
// root: vendored or generated markdown is retrieval noise (the reason
// ADR-0055 rejected all-repo scope).
func SkipDir(name string) bool { return skipDirs[name] }

// ParseConfig decodes the .lema/config.json bytes into a Config. A malformed
// file yields the zero value and an error; callers decide whether to warn or
// fail — the local walker warns and proceeds with default roots (a config
// typo must not brick the engine), and the server-side scanner surfaces it as
// a scan warning for the same reason.
func ParseConfig(b []byte) (Config, error) {
	var c Config
	if len(b) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse .lema/config.json: %w", err)
	}
	return c, nil
}

// IsMarkdown reports whether a repo-relative path is a markdown file.
func IsMarkdown(rel string) bool {
	return strings.HasSuffix(strings.ToLower(rel), ".md")
}

// InScope is the policy predicate: does this repo-relative, slash-separated
// path belong to the project-doc set under cfg?
//
// A path is in scope when it is markdown, is not carved out by a config
// exclude prefix, descends through no skip-dir, and sits under a default
// directory root, equals a default root file, or is covered by a config
// include entry (which is why the include list is the opt-in escape hatch for
// an unusual repo).
//
// Deliberately says nothing about file SIZE: that needs a stat or an API
// response, and lives with whichever caller can answer it (MaxDocBytes).
func InScope(rel string, cfg Config) bool {
	rel = normalizeRel(rel)
	if rel == "" || !IsMarkdown(rel) {
		return false
	}
	if Excluded(rel, cfg) {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if SkipDir(seg) {
			return false
		}
	}
	for _, d := range defaultDirs {
		if underDir(rel, d) {
			return true
		}
	}
	for _, f := range defaultFiles {
		if rel == f {
			return true
		}
	}
	for _, inc := range cfg.Docs.Include {
		inc = normalizeRel(inc)
		if inc == "" {
			continue
		}
		// An include entry names either a directory (walked recursively) or a
		// single file. Path-only, we cannot tell which — so both readings are
		// accepted, which is exactly the caller's intent either way.
		if rel == inc || underDir(rel, inc) {
			return true
		}
	}
	return false
}

// Excluded reports whether a config exclude entry carves out this path.
// Prefixes, not globs: stdlib has no '**' matcher and a glob dependency is
// not warranted (the ADR-0055 note on Config).
func Excluded(rel string, cfg Config) bool {
	rel = normalizeRel(rel)
	for _, ex := range cfg.Docs.Exclude {
		ex = normalizeRel(ex)
		if ex != "" && (rel == ex || underDir(rel, ex)) {
			return true
		}
	}
	return false
}

// OutOfTree reports whether a config include entry escapes the repo root
// (".." segments or an absolute path). The local walker refuses these because
// they would read outside the checkout; the server-side scanner refuses them
// because a tree listing has no such paths to match, so an escape attempt can
// only be a mistake or a probe.
func OutOfTree(inc string) bool {
	inc = strings.TrimSpace(filepath.ToSlash(inc))
	if strings.HasPrefix(inc, "/") {
		return true
	}
	for _, seg := range strings.Split(normalizeRel(inc), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func normalizeRel(p string) string {
	return strings.Trim(filepath.ToSlash(strings.TrimSpace(p)), "/")
}

func underDir(rel, dir string) bool {
	return dir != "" && strings.HasPrefix(rel, dir+"/")
}
