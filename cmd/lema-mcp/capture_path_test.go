package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// Capture must not die in git worktrees. lema's own build-pain retro found the
// dominant dogfood failure: agents build in linked worktrees, where the
// default relative `.lema/decisions.jsonl` resolves to the WORKTREE's checkout
// — a frozen copy of the store from branch time. Writes fork there and are
// destroyed with the worktree; reads enforce a stale snapshot ("9 days, 0
// captures" — 21 worktrees carried byte-identical 8-line stores). The repo has
// ONE decision record; every checkout of it must read and write the same one.
// So a relative capture path inside a LINKED worktree anchors to the MAIN
// checkout root (via git rev-parse --git-common-dir); everything else — main
// checkout, plain directory, explicit absolute path — is untouched.

// gitHere runs git in dir, failing the test on error.
func gitHere(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		// A host gitconfig must not leak in (e.g. worktree extensions, hooks).
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// repoWithWorktree builds a real main checkout + linked worktree pair.
func repoWithWorktree(t *testing.T) (mainRoot, worktreeRoot string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	base := t.TempDir()
	// macOS: /var symlinks to /private/var — resolve so path comparisons hold.
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	mainRoot = filepath.Join(base, "repo")
	if err := os.MkdirAll(mainRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	gitHere(t, mainRoot, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(mainRoot, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitHere(t, mainRoot, "add", ".")
	gitHere(t, mainRoot, "commit", "-q", "-m", "init")
	worktreeRoot = filepath.Join(base, "wt")
	gitHere(t, mainRoot, "worktree", "add", "-q", worktreeRoot, "-b", "feature")
	return mainRoot, worktreeRoot
}

func TestResolveCaptureFileInLinkedWorktreeAnchorsToMainCheckout(t *testing.T) {
	mainRoot, worktreeRoot := repoWithWorktree(t)
	want := filepath.Join(mainRoot, ".lema", "decisions.jsonl")

	t.Run("worktree root", func(t *testing.T) {
		t.Chdir(worktreeRoot)
		if got := resolveCaptureFile(".lema/decisions.jsonl"); got != want {
			t.Errorf("resolveCaptureFile in worktree = %q, want the MAIN checkout store %q (a worktree-local store forks the record and dies with the worktree)", got, want)
		}
	})
	t.Run("worktree subdirectory", func(t *testing.T) {
		sub := filepath.Join(worktreeRoot, "apps", "api")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(sub)
		if got := resolveCaptureFile(".lema/decisions.jsonl"); got != want {
			t.Errorf("resolveCaptureFile in worktree subdir = %q, want %q", got, want)
		}
	})
}

func TestResolveCaptureFileUnchangedOutsideLinkedWorktrees(t *testing.T) {
	mainRoot, worktreeRoot := repoWithWorktree(t)

	t.Run("main checkout", func(t *testing.T) {
		t.Chdir(mainRoot)
		if got := resolveCaptureFile(".lema/decisions.jsonl"); got != ".lema/decisions.jsonl" {
			t.Errorf("main checkout must keep the relative path (cwd-resolved), got %q", got)
		}
	})
	t.Run("plain directory", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if got := resolveCaptureFile(".lema/decisions.jsonl"); got != ".lema/decisions.jsonl" {
			t.Errorf("non-git directory must keep the relative path, got %q", got)
		}
	})
	t.Run("explicit absolute path wins everywhere", func(t *testing.T) {
		t.Chdir(worktreeRoot)
		abs := filepath.Join(t.TempDir(), "elsewhere.jsonl")
		if got := resolveCaptureFile(abs); got != abs {
			t.Errorf("an explicit absolute --capture-file must never be redirected, got %q", got)
		}
	})
	t.Run("empty path untouched", func(t *testing.T) {
		if got := resolveCaptureFile(""); got != "" {
			t.Errorf("empty path = %q, want empty", got)
		}
	})
}

// The worktree anchoring is what makes a capture written FROM a worktree
// session land in (and enforce from) the one true store: prove it end to end
// through the CaptureStore, not just the path string.
func TestWorktreeCaptureLandsInMainCheckoutStore(t *testing.T) {
	mainRoot, worktreeRoot := repoWithWorktree(t)
	t.Chdir(worktreeRoot)

	storePath := resolveCaptureFile(".lema/decisions.jsonl")
	if err := os.MkdirAll(filepath.Dir(storePath), 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := source.NewCaptureStore(storePath)
	if err != nil {
		t.Fatalf("NewCaptureStore: %v", err)
	}
	if _, err := store.Record(source.DecisionRecord{Title: "worktree capture", Chosen: "the main checkout store"}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if _, err := os.Stat(filepath.Join(mainRoot, ".lema", "decisions.jsonl")); err != nil {
		t.Fatalf("capture from a worktree session did not land in the main checkout store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktreeRoot, ".lema", "decisions.jsonl")); err == nil {
		t.Fatal("capture forked a worktree-local store — the exact orphaned-record bug this fix kills")
	}
}
