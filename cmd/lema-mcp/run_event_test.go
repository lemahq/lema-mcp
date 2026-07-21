package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStderr redirects os.Stderr for the duration of f and returns what was
// written. The package's tests run sequentially (no t.Parallel), so the
// process-global swap is safe.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	f()
	_ = w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	return buf.String()
}

func TestSanitizeTabID(t *testing.T) {
	if got := sanitizeTabID("session-2"); got != "session-2" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeTabID("weird id!"); got != "weird-id" {
		t.Fatalf("got %q", got)
	}
	if got := sanitizeTabID("!!!"); got != "default" {
		t.Fatalf("empty sanitize should default, got %q", got)
	}
}

func TestDistillCheckpointFromEvents(t *testing.T) {
	events := []runSpoolEvent{
		{Kind: "user_prompt", Prompt: "first task", PhysicalSessionID: "phys-1"},
		{Kind: "tool_use", ToolName: "Edit", FilePath: "src/a.go"},
		{Kind: "user_prompt", Prompt: "second task"},
		{Kind: "tool_use", ToolName: "Write", FilePath: "src/b.go"},
	}
	cp := distillCheckpoint(events, "main")
	if cp.LogicalRunID != "main" {
		t.Fatalf("logical run = %q", cp.LogicalRunID)
	}
	if len(cp.RecentPrompts) != 2 {
		t.Fatalf("prompts = %v", cp.RecentPrompts)
	}
	if len(cp.FilesTouched) != 2 {
		t.Fatalf("files = %v", cp.FilesTouched)
	}
	if cp.PhysicalSessionID != "phys-1" {
		t.Fatalf("physical session = %q", cp.PhysicalSessionID)
	}
	if !strings.Contains(cp.Summary, "second task") {
		t.Fatalf("summary missing last prompt: %q", cp.Summary)
	}
}

func TestWriteAndReadCheckpoint(t *testing.T) {
	dir := t.TempDir()
	tab := "main"
	cp := distillCheckpoint([]runSpoolEvent{
		{Kind: "user_prompt", Prompt: "ship run-ledger"},
	}, tab)
	if err := writeCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	got, ok := readCheckpoint(dir, tab)
	if !ok {
		t.Fatal("expected checkpoint")
	}
	if got.Summary != cp.Summary {
		t.Fatalf("summary mismatch: %q vs %q", got.Summary, cp.Summary)
	}
}

func TestAppendAndReadSpool(t *testing.T) {
	dir := t.TempDir()
	tab := "session-3"
	ev := runSpoolEvent{At: "2026-06-29T00:00:00Z", TabID: tab, Kind: "user_prompt", Prompt: "hello"}
	if err := appendSpoolEvent(dir, tab, ev); err != nil {
		t.Fatal(err)
	}
	events, err := readSpoolEvents(dir, tab)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Prompt != "hello" {
		t.Fatalf("events = %+v", events)
	}
}

func TestFormatInjectBlock(t *testing.T) {
	block := formatInjectBlock(runCheckpoint{
		LogicalRunID:  "main",
		Summary:       "last prompt: fix bug",
		RecentPrompts: []string{"fix bug"},
		FilesTouched:  []string{"a.go"},
	})
	if !strings.Contains(block, "run-ledger checkpoint") {
		t.Fatalf("missing header: %q", block)
	}
	if !strings.Contains(block, "fix bug") {
		t.Fatalf("missing prompt: %q", block)
	}
	if !strings.Contains(block, "a.go") {
		t.Fatalf("missing file: %q", block)
	}
}

func TestRunEventSpoolDirOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(runSpoolDirEnv, dir)
	got, err := runEventSpoolDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("got %q want %q", got, dir)
	}
}

func TestRunEventEnabled(t *testing.T) {
	t.Setenv(runLedgerEnv, "1")
	if !runEventEnabled() {
		t.Fatal("expected enabled")
	}
	t.Setenv(runLedgerEnv, "0")
	if runEventEnabled() {
		t.Fatal("expected disabled")
	}
}

func TestCheckpointPathLayout(t *testing.T) {
	dir := "/tmp/spool"
	tab := "main"
	if got := checkpointPath(dir, tab); got != filepath.Join(dir, "main.checkpoint.json") {
		t.Fatalf("checkpoint path = %q", got)
	}
	if got := spoolPath(dir, tab); got != filepath.Join(dir, "main.jsonl") {
		t.Fatalf("spool path = %q", got)
	}
}

func TestRunEventTabIDFromEnv(t *testing.T) {
	t.Setenv(runTabEnv, "session-5")
	if got := runEventTabID(); got != "session-5" {
		t.Fatalf("got %q", got)
	}
	t.Setenv(runTabEnv, "")
	if got := runEventTabID(); got != "default" {
		t.Fatalf("got %q", got)
	}
}

func TestRunRunEventSessionStartNoCheckpoint(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(runLedgerEnv, "1")
	t.Setenv(runSpoolDirEnv, dir)
	t.Setenv(runTabEnv, "main")
	runRunEvent([]string{"SessionStart"})
	if _, err := os.Stat(spoolPath(dir, "main")); err != nil {
		t.Fatalf("expected spool file: %v", err)
	}
}

// TestRunEventDeprecationNotice pins the ADR-0110 deprecation-note-first signal:
// SessionStart surfaces the deprecation notice to stderr even when the feature
// is DISABLED (so a dormant-but-wired run-event still gets warned to migrate to
// `collect`), and non-boundary hook events stay silent (one notice per session,
// not per hook).
func TestRunEventDeprecationNotice(t *testing.T) {
	// Disabled: run-event is a no-op today, but the boundary still warns.
	t.Setenv(runLedgerEnv, "0")
	got := captureStderr(t, func() { runRunEvent([]string{"SessionStart"}) })
	if !strings.Contains(got, "DEPRECATED") || !strings.Contains(got, "collect") {
		t.Fatalf("SessionStart must surface the deprecation notice, got %q", got)
	}

	// A non-boundary event must not repeat the notice (no per-hook spam).
	quiet := captureStderr(t, func() { runRunEvent([]string{"PostToolUse"}) })
	if strings.Contains(quiet, "DEPRECATED") {
		t.Fatalf("non-SessionStart events must stay silent, got %q", quiet)
	}
}
