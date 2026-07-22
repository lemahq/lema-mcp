package main

import (
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
