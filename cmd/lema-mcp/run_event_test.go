package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
