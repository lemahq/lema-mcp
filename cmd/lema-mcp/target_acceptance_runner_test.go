package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTargetContextAcceptanceRunnerListsEveryLocalCase(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "target-context-acceptance.sh"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(script, "--list").CombinedOutput()
	if err != nil {
		t.Fatalf("acceptance runner --list: %v\n%s", err, out)
	}
	want := []string{
		"one-repo",
		"parallel-repos",
		"two-users",
		"cross-repo-work-unit",
		"ambiguous-project",
		"stale-override",
		"worktree",
		"fork-upstream-distinct",
		"repository-rename",
		"organization-transfer",
		"monorepo",
		"no-remote",
		"enterprise-host",
		"hidden-leaf",
		"legacy-uuid",
	}
	got := strings.Fields(string(out))
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("local acceptance cases:\n got %q\nwant %q", got, want)
	}
	for _, forbidden := range []string{"remote-http", "devin"} {
		if strings.Contains(strings.ToLower(string(out)), forbidden) {
			t.Fatalf("Phase 8 case %q entered the local acceptance boundary: %s", forbidden, out)
		}
	}
}

func TestTargetContextAcceptanceRunnerSmokeRestartsCandidateAndCallsStateBrief(t *testing.T) {
	dir := t.TempDir()
	adr := filepath.Join(dir, "0001-smoke.md")
	if err := os.WriteFile(adr, []byte("---\nstatus: accepted\n---\n# Smoke\n\n## Decision\nUse the acceptance smoke.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "target-context-acceptance.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script, "--smoke-only")
	cmd.Env = append(os.Environ(), "LEMA_ACCEPTANCE_ADR_DIR="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("acceptance runner --smoke-only: %v\n%s", err, out)
	}
	got := string(out)
	if strings.Count(got, "candidate: fresh stdio process") != 2 {
		t.Fatalf("candidate was not restarted exactly once:\n%s", got)
	}
	if strings.Count(got, "safe State Brief unavailable call passed") != 2 {
		t.Fatalf("both candidate processes did not validate a safe State Brief call:\n%s", got)
	}
}
