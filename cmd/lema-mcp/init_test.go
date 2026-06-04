package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInitWritesAllThreeFiles(t *testing.T) {
	dir := t.TempDir()
	if err := runInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".mcp.json", "AGENTS.md", filepath.Join(".claude", "settings.json")} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to exist: %v", f, err)
		}
	}

	agents, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(agents), "record_decision") {
		t.Error("AGENTS.md is missing the capture protocol")
	}

	var mcp map[string]any
	mj, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	json.Unmarshal(mj, &mcp)
	servers, _ := mcp["mcpServers"].(map[string]any)
	if _, ok := servers["lema"]; !ok {
		t.Error(".mcp.json did not register the lema server")
	}
}

func TestRunInitIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := runInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	snap := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return string(b)
	}
	before := snap(".mcp.json") + snap("AGENTS.md") + snap(filepath.Join(".claude", "settings.json"))
	if err := runInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	after := snap(".mcp.json") + snap("AGENTS.md") + snap(filepath.Join(".claude", "settings.json"))
	if before != after {
		t.Error("second init changed files; expected idempotent no-op")
	}
}

func TestRunInitPreservesExistingConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"),
		[]byte(`{"mcpServers":{"other":{"command":"foo"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	var mcp map[string]any
	mj, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	json.Unmarshal(mj, &mcp)
	servers, _ := mcp["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Error("init clobbered the pre-existing 'other' server")
	}
	if _, ok := servers["lema"]; !ok {
		t.Error("init did not add the lema server alongside the existing one")
	}
}

func TestRunInitRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{dir}); err == nil {
		t.Error("expected init to refuse a malformed .mcp.json rather than discard it")
	}
}

func TestEnsurePreToolUseHook(t *testing.T) {
	path := t.TempDir() + "/settings.json"

	changed, err := ensurePreToolUseHook(path)
	if err != nil || !changed {
		t.Fatalf("first install: changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"PreToolUse"`) ||
		!strings.Contains(string(b), guardMarker()) ||
		!strings.Contains(string(b), `"Edit|Write"`) {
		t.Fatalf("hook not written correctly: %s", b)
	}

	// Idempotent: second call changes nothing.
	if changed, _ := ensurePreToolUseHook(path); changed {
		t.Fatal("second install should be a no-op")
	}
}

// TestInitRepoInstallsGuard confirms initRepo wires the guard end to end — the
// guard hook lands in .claude/settings.json alongside the existing commit reminder.
func TestInitRepoInstallsGuard(t *testing.T) {
	dir := t.TempDir()
	if _, err := initRepo(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	s := string(b)
	if !strings.Contains(s, `"PreToolUse"`) || !strings.Contains(s, guardMarker()) {
		t.Fatalf("initRepo did not install the guard hook: %s", s)
	}
	// The existing commit reminder must survive alongside it.
	if !strings.Contains(s, `"PostToolUse"`) || !strings.Contains(s, reminderMarker) {
		t.Fatalf("initRepo dropped the commit reminder: %s", s)
	}
	// And the capture nudge (ADR-0054) is installed too.
	if !strings.Contains(s, captureNudgeMarker()) {
		t.Fatalf("initRepo did not install the capture-nudge hook: %s", s)
	}
}

func TestRemoveGuardHook(t *testing.T) {
	path := t.TempDir() + "/settings.json"
	if _, err := ensurePreToolUseHook(path); err != nil {
		t.Fatal(err)
	}

	changed, err := removeGuardHook(path)
	if err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), guardMarker()) {
		t.Fatalf("guard hook not removed: %s", b)
	}
	// Idempotent: removing again is a no-op.
	if changed, _ := removeGuardHook(path); changed {
		t.Fatal("second remove should be a no-op")
	}
}

func TestEnsureCaptureNudgeHook(t *testing.T) {
	path := t.TempDir() + "/settings.json"

	changed, err := ensureCaptureNudgeHook(path)
	if err != nil || !changed {
		t.Fatalf("first install: changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"PostToolUse"`) ||
		!strings.Contains(string(b), captureNudgeMarker()) ||
		!strings.Contains(string(b), `"Edit|Write|MultiEdit"`) {
		t.Fatalf("nudge hook not written correctly: %s", b)
	}
	// Idempotent: second call changes nothing.
	if changed, _ := ensureCaptureNudgeHook(path); changed {
		t.Fatal("second install should be a no-op")
	}
}

func TestRemoveCaptureNudgeHook(t *testing.T) {
	path := t.TempDir() + "/settings.json"
	// Install BOTH the commit reminder and the nudge (both PostToolUse) — remove
	// must drop ONLY the nudge.
	if _, err := ensureClaudeHook(path); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureCaptureNudgeHook(path); err != nil {
		t.Fatal(err)
	}

	changed, err := removeCaptureNudgeHook(path)
	if err != nil || !changed {
		t.Fatalf("remove: changed=%v err=%v", changed, err)
	}
	s := func() string { b, _ := os.ReadFile(path); return string(b) }()
	if strings.Contains(s, captureNudgeMarker()) {
		t.Fatalf("nudge hook not removed: %s", s)
	}
	// The commit reminder must survive — removeCaptureNudgeHook is surgical.
	if !strings.Contains(s, reminderMarker) {
		t.Fatalf("removeCaptureNudgeHook clobbered the commit reminder: %s", s)
	}
	// Idempotent: removing again is a no-op.
	if changed, _ := removeCaptureNudgeHook(path); changed {
		t.Fatal("second remove should be a no-op")
	}
}
