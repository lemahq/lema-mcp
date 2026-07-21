package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// F4's contract: run boundaries distill deterministically into a per-project
// checkpoint keyed on cwd (rung-4-style continuity — never a tab id), and a
// later SessionStart from the same project injects it with honest provenance
// (producing run + age); an expired checkpoint is never injected.

func mkEnv(runID, kind string, payload map[string]string) collectorEnvelope {
	return collectorEnvelope{
		RunID:   runID,
		TS:      time.Now().UTC().Format(time.RFC3339),
		Kind:    kind,
		Payload: payload,
		Evidence: map[string]string{
			"harness": "claude-code",
			"cwd":     "/repo/proj",
		},
	}
}

func TestDistillEnvelopesSelectsDeterministically(t *testing.T) {
	envs := []collectorEnvelope{
		mkEnv("r1", "session_start", nil),
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "first ask"}),
		mkEnv("r1", "tool_use", map[string]string{"tool_name": "Edit", "file_path": "a.go"}),
		mkEnv("r1", "tool_use", map[string]string{"tool_name": "Edit", "file_path": "a.go"}),
		mkEnv("r1", "tool_use", map[string]string{"tool_name": "Write", "file_path": "b.go"}),
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "second ask"}),
	}
	cp := distillEnvelopes(envs, "/repo/proj")
	if cp.RunID != "r1" || cp.CWD != "/repo/proj" || cp.EventCount != 6 {
		t.Fatalf("cp = %#v", cp)
	}
	if len(cp.FilesTouched) != 2 || cp.FilesTouched[0] != "a.go" || cp.FilesTouched[1] != "b.go" {
		t.Fatalf("files must dedupe in order, got %v", cp.FilesTouched)
	}
	if len(cp.RecentPrompts) != 2 || cp.RecentPrompts[1] != "second ask" {
		t.Fatalf("prompts = %v", cp.RecentPrompts)
	}
	if !strings.Contains(cp.Summary, "second ask") || !strings.Contains(cp.Summary, "2 file(s)") {
		t.Fatalf("summary = %q", cp.Summary)
	}
}

func TestCheckpointRoundTripAndExpiry(t *testing.T) {
	dir := t.TempDir()
	cp := distillEnvelopes([]collectorEnvelope{mkEnv("r1", "user_prompt", map[string]string{"prompt": "x"})}, "/repo/proj")
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now()); !ok {
		t.Fatal("fresh checkpoint must read back")
	}
	if _, ok := readCollectorCheckpoint(dir, "/other", time.Now()); ok {
		t.Fatal("a different project must not see this checkpoint")
	}
	if _, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now().Add(collectorTTL+time.Hour)); ok {
		t.Fatal("an expired checkpoint must never inject")
	}
}

func TestCheckpointOnBoundaryWritesOnlyOnBoundaries(t *testing.T) {
	dir := t.TempDir()
	for _, ev := range []collectorEnvelope{
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "work work"}),
		mkEnv("r1", "tool_use", map[string]string{"file_path": "a.go"}),
	} {
		if err := appendEnvelope(dir, ev); err != nil {
			t.Fatal(err)
		}
		checkpointOnBoundary(dir, ev) // non-boundary kinds: no checkpoint
	}
	if _, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now()); ok {
		t.Fatal("non-boundary events must not checkpoint")
	}
	// runCollect distills BEFORE appending the boundary marker (run_event.go's
	// order): the stop itself is not activity and stays out of its checkpoint.
	stop := mkEnv("r1", "stop", nil)
	checkpointOnBoundary(dir, stop)
	if err := appendEnvelope(dir, stop); err != nil {
		t.Fatal(err)
	}
	cp, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now())
	if !ok {
		t.Fatal("stop must write the checkpoint")
	}
	if cp.RunID != "r1" || cp.EventCount != 2 {
		t.Fatalf("cp = %#v (boundary marker must not count as activity)", cp)
	}
}

func TestCheckpointKeyDisambiguatesSlashDashSiblings(t *testing.T) {
	if checkpointKey("/repo/apps/web") == checkpointKey("/repo/apps-web") {
		t.Fatal("slash/dash sibling projects must not share a checkpoint file")
	}
}

func TestCheckpointFiltersForeignCwdEnvelopes(t *testing.T) {
	dir := t.TempDir()
	foreign := mkEnv("r1", "tool_use", map[string]string{"file_path": "secret.go"})
	foreign.Evidence["cwd"] = "/elsewhere"
	for _, ev := range []collectorEnvelope{
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "here"}),
		foreign,
	} {
		if err := appendEnvelope(dir, ev); err != nil {
			t.Fatal(err)
		}
	}
	checkpointOnBoundary(dir, mkEnv("r1", "stop", nil))
	cp, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now())
	if !ok {
		t.Fatal("checkpoint expected")
	}
	if cp.EventCount != 1 || len(cp.FilesTouched) != 0 {
		t.Fatalf("a resumed run's other-project envelopes must not leak in: %#v", cp)
	}
}

func TestReadCheckpointRefusesForeignCWD(t *testing.T) {
	dir := t.TempDir()
	cp := distillEnvelopes([]collectorEnvelope{mkEnv("r1", "user_prompt", map[string]string{"prompt": "x"})}, "/repo/proj")
	cp.CWD = "/somewhere/else" // simulate a mis-filed or corrupted checkpoint
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCollectorCheckpoint(dir, "/somewhere/else", time.Now()); !ok {
		t.Fatal("sanity: the checkpoint reads back under its own cwd")
	}
	if _, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now()); ok {
		t.Fatal("a checkpoint whose stored cwd differs must never be served")
	}
}

// captureStdout lives in settle_test.go.

func TestInjectOnStartEmitsAdditionalContext(t *testing.T) {
	dir := t.TempDir()
	cp := distillEnvelopes([]collectorEnvelope{
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "ship F4"}),
		mkEnv("r1", "tool_use", map[string]string{"file_path": "collector.go"}),
	}, "/repo/proj")
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		injectOnStart(dir, mkEnv("r2-new-session", "session_start", nil))
	})
	var hook guardOutput
	if err := json.Unmarshal([]byte(out), &hook); err != nil {
		t.Fatalf("stdout must be one hook JSON object, got %q: %v", out, err)
	}
	ctx := hook.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "ship F4") || !strings.Contains(ctx, "collector.go") {
		t.Fatalf("injected context missing distilled state: %q", ctx)
	}
	if !strings.Contains(ctx, "run r1") {
		t.Fatalf("injection must attribute the producing run: %q", ctx)
	}
}

func TestInjectOnStartSilentWhenNoCheckpoint(t *testing.T) {
	dir := t.TempDir()
	out := captureStdout(t, func() {
		injectOnStart(dir, mkEnv("r9", "session_start", nil))
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("no checkpoint → no stdout, got %q", out)
	}
}
