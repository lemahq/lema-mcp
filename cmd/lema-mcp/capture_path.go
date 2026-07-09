package main

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// capture_path.go — one decision record per repo, across every checkout of it.
//
// The default capture store path is the RELATIVE `.lema/decisions.jsonl`,
// resolved against the process cwd. Inside a LINKED git worktree that resolves
// to the worktree's own checkout — a copy of the store frozen at branch time —
// so worktree sessions read a stale snapshot and their writes fork into a file
// that is destroyed with the worktree. lema's own build-pain retro measured
// the damage: ~100% of building happens in worktrees, and capture was dead
// there (21 worktrees carrying byte-identical frozen stores; "9 days, 0
// captures"). The repo has ONE decision record; a worktree is a checkout of
// the repo, not a different repo.
//
// resolveCaptureFile therefore anchors a relative capture path at the MAIN
// checkout root whenever the cwd sits inside a linked worktree, derived from
// `git rev-parse --git-common-dir` (the shared .git directory all worktrees of
// a repo point back to). Everything else is deliberately untouched:
//   - an explicit ABSOLUTE --capture-file always wins (tests, custom setups);
//   - the main checkout and plain directories keep today's cwd-relative
//     behaviour;
//   - a submodule is NOT a linked worktree (its git-dir equals its common
//     dir), so it keeps its own store rather than leaking into the
//     superproject's;
//   - a bare main repo has no main checkout to anchor to, so it is left alone;
//   - any git failure fails open to the unresolved path (the guard hook must
//     never block on this).

// resolveCaptureFile returns the path the capture store should actually use
// for a configured (possibly default) capture-file path.
func resolveCaptureFile(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	root := linkedWorktreeMainRoot(".")
	if root == "" {
		return path
	}
	return filepath.Join(root, path)
}

// linkedWorktreeMainRoot returns the main checkout root when dir is inside a
// LINKED git worktree, and "" everywhere else (main checkout, plain dir,
// submodule, bare repo, no git).
func linkedWorktreeMainRoot(dir string) string {
	out, err := gitRevParseDirs(dir)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		return ""
	}
	gitDir := strings.TrimSpace(lines[0])
	commonDir := strings.TrimSpace(lines[1])
	if gitDir == "" || commonDir == "" {
		return ""
	}
	// --git-common-dir may print relative to dir (unlike --absolute-git-dir).
	if !filepath.IsAbs(commonDir) {
		abs, err := filepath.Abs(filepath.Join(dir, commonDir))
		if err != nil {
			return ""
		}
		commonDir = abs
	}
	commonDir = filepath.Clean(commonDir)
	if filepath.Clean(gitDir) == commonDir {
		return "" // main checkout (or submodule): not a linked worktree
	}
	if filepath.Base(commonDir) != ".git" {
		return "" // bare main repo: no main checkout to anchor to
	}
	return filepath.Dir(commonDir)
}

// gitRevParseDirs asks git for the two directories that distinguish a linked
// worktree: its private git-dir and the repo's shared common dir. Split out
// only so it stays one exec for both values.
func gitRevParseDirs(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--absolute-git-dir", "--git-common-dir")
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
