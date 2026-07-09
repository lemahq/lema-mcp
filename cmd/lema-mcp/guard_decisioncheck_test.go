package main

import (
	"strings"
	"testing"
)

// The guard's tier-1 decision-check path: an edit that reintroduces the
// ADR-0111 env-gate pattern (the preserved 05c033f8 fixture) must produce a
// PreToolUse nudge citing ADR-0111; a clean edit must produce nothing (the
// zero-FP contract carried through the guard).

func TestDecisionCheckGuard_FiresOnFixtureEdit(t *testing.T) {
	in := map[string]any{
		"file_path":  "/Users/x/lema/apps/web/lib/launch-mode.ts",
		"new_string": "export function recordConflictsEnabled(): boolean {\n  return process.env.LEMA_RECORD_CONFLICTS_ENABLED === 'true';\n}",
	}
	out := decisionCheckGuard(in, guardModeContext)
	if out == nil {
		t.Fatal("guard must fire on the ADR-0111 fixture edit")
	}
	ctx := out.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "ADR-0111") {
		t.Errorf("nudge must cite ADR-0111, got %q", ctx)
	}
	// Context mode never changes the permission (the human's Edit confirmation
	// is never skipped) — same rail as evaluateGuard.
	if out.HookSpecificOutput.PermissionDecision != "" {
		t.Errorf("context mode must not set a permissionDecision, got %q", out.HookSpecificOutput.PermissionDecision)
	}
}

func TestDecisionCheckGuard_AskModePrompts(t *testing.T) {
	in := map[string]any{
		"file_path":  "apps/web/lib/launch-mode.ts",
		"new_string": "return process.env.LEMA_FOO_ENABLED === 'true';",
	}
	out := decisionCheckGuard(in, guardModeAsk)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("ask mode must prompt the human on a fixture hit, got %+v", out)
	}
}

func TestDecisionCheckGuard_SilentOnCleanEdit(t *testing.T) {
	in := map[string]any{
		"file_path":  "apps/web/lib/launch-mode.ts",
		"new_string": "export function homePath(): string { return '/ask'; }",
	}
	if out := decisionCheckGuard(in, guardModeContext); out != nil {
		t.Fatalf("clean edit must not fire, got %+v", out)
	}
}

func TestDecisionCheckGuard_SilentOffScopeFile(t *testing.T) {
	// The pattern in a non-launch-mode file (an api kill-switch) is out of scope.
	in := map[string]any{
		"file_path":  "apps/api/internal/api/agent_public.go",
		"new_string": `os.Getenv("LEMA_AGENT_DEMO_ENABLED")`,
	}
	if out := decisionCheckGuard(in, guardModeContext); out != nil {
		t.Fatalf("off-scope file must not fire, got %+v", out)
	}
}

func TestChangeFromToolInput_MultiEdit(t *testing.T) {
	in := map[string]any{
		"file_path": "apps/web/lib/launch-mode.ts",
		"edits": []any{
			map[string]any{"new_string": "line a"},
			map[string]any{"new_string": "process.env.LEMA_X_ENABLED"},
		},
	}
	c := changeFromToolInput(in)
	if c.Path != "apps/web/lib/launch-mode.ts" || !strings.Contains(c.NewText, "LEMA_X_ENABLED") {
		t.Fatalf("MultiEdit new_strings must be gathered, got %+v", c)
	}
}
