package main

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The collector's contract (F3): run identity comes from the ADAPTER — the
// Claude Code adapter keys on the harness session_id and never fabricates an
// identity for an event that lacks one. These tests pin that, the envelope
// shape, and the spool's expiring semantics.

func TestClaudeCodeAdapterNormalizesToolUse(t *testing.T) {
	stdin := []byte(`{
		"session_id": "abc-123",
		"transcript_path": "/tmp/t.jsonl",
		"cwd": "/repo",
		"tool_name": "Edit",
		"tool_input": {"file_path": "internal/api/x.go", "old_string": "a"}
	}`)
	ev, ok := claudeCodeAdapter{}.normalize("PostToolUse", stdin)
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.RunID != "abc-123" {
		t.Fatalf("run_id = %q, want the harness session_id", ev.RunID)
	}
	if ev.Kind != "tool_use" {
		t.Fatalf("kind = %q, want tool_use", ev.Kind)
	}
	if ev.Payload["tool_name"] != "Edit" || ev.Payload["file_path"] != "internal/api/x.go" {
		t.Fatalf("payload = %#v", ev.Payload)
	}
	if _, leaked := ev.Payload["old_string"]; leaked {
		t.Fatal("payload must carry only normalized fields, not raw tool_input")
	}
	if ev.Evidence["harness"] != "claude-code" || ev.Evidence["hook_event"] != "PostToolUse" {
		t.Fatalf("evidence = %#v", ev.Evidence)
	}
	if ev.Evidence["transcript_path"] != "/tmp/t.jsonl" {
		t.Fatalf("evidence must point at the raw transcript, got %#v", ev.Evidence)
	}
	if _, err := time.Parse(time.RFC3339, ev.TS); err != nil {
		t.Fatalf("ts not RFC3339: %v", err)
	}
}

func TestClaudeCodeAdapterKindMapping(t *testing.T) {
	cases := map[string]string{
		"SessionStart":     "session_start",
		"UserPromptSubmit": "user_prompt",
		"PreToolUse":       "tool_use",
		"PostToolUse":      "tool_use",
		"Stop":             "stop",
		"PreCompact":       "pre_compact",
		"SessionEnd":       "session_end",
		"SomethingNew":     "somethingnew",
	}
	for hookEvent, want := range cases {
		ev, ok := claudeCodeAdapter{}.normalize(hookEvent, []byte(`{"session_id":"s"}`))
		if !ok || ev.Kind != want {
			t.Fatalf("%s → kind %q (ok=%v), want %q", hookEvent, ev.Kind, ok, want)
		}
	}
}

func TestClaudeCodeAdapterSkipsWithoutSessionID(t *testing.T) {
	for _, stdin := range []string{`{}`, `{"session_id":"  "}`, `not json`} {
		if _, ok := (claudeCodeAdapter{}).normalize("SessionStart", []byte(stdin)); ok {
			t.Fatalf("stdin %q: adapter must skip rather than fabricate run identity", stdin)
		}
	}
}

func TestCollectorAdapterForUnknownHarness(t *testing.T) {
	if collectorAdapterFor("codex") != nil {
		t.Fatal("codex adapter must not exist until it is implemented for real")
	}
	if collectorAdapterFor("claude-code") == nil {
		t.Fatal("claude-code adapter missing")
	}
}

func TestAppendAndReadRunEnvelopes(t *testing.T) {
	dir := t.TempDir()
	for _, kind := range []string{"session_start", "tool_use"} {
		if err := appendEnvelope(dir, collectorEnvelope{RunID: "run-1", TS: "2026-07-21T00:00:00Z", Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := readRunEnvelopes(dir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != "session_start" || got[1].Kind != "tool_use" {
		t.Fatalf("got %#v", got)
	}
	if missing, err := readRunEnvelopes(dir, "no-such-run"); err != nil || missing != nil {
		t.Fatalf("missing run must read as empty, got %v / %v", missing, err)
	}
}

func TestPruneExpiredRuns(t *testing.T) {
	dir := t.TempDir()
	fresh := collectorRunPath(dir, "fresh")
	stale := collectorRunPath(dir, "stale")
	for _, p := range []string{fresh, stale} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-collectorTTL - time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	pruneExpiredRuns(dir, time.Now())
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale run file must be pruned")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh run file must survive")
	}
}

// runCollectWithStdin runs the subcommand entrypoint with stdin replaced.
func runCollectWithStdin(t *testing.T, payload string, args ...string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig }()
	if _, err := w.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	w.Close()
	runCollect(args)
}

func TestRunCollectSpoolsEnvelope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(collectorDirEnv, dir)
	runCollectWithStdin(t, `{"session_id":"sess-9","prompt":"fix the bug"}`, "claude-code", "UserPromptSubmit")
	got, err := readRunEnvelopes(dir, "sess-9")
	if err != nil || len(got) != 1 {
		t.Fatalf("want 1 envelope, got %v / %v", got, err)
	}
	if got[0].Kind != "user_prompt" || got[0].Payload["prompt"] != "fix the bug" {
		t.Fatalf("got %#v", got[0])
	}
}

func TestRunCollectFailOpen(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(collectorDirEnv, dir)
	// Unknown harness, missing args, bad stdin: all must be silent no-ops.
	runCollectWithStdin(t, `{"session_id":"s"}`, "codex", "SessionStart")
	runCollect([]string{"claude-code"})
	runCollectWithStdin(t, `garbage`, "claude-code", "SessionStart")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("no spool file may be written on any fail-open path, found %d", len(entries))
	}
}

func TestEnvelopeWireShape(t *testing.T) {
	ev := collectorEnvelope{
		RunID:    "r",
		TS:       "2026-07-21T00:00:00Z",
		Kind:     "session_start",
		Evidence: map[string]string{"harness": "claude-code"},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"run_id", "ts", "kind", "evidence"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("wire envelope missing %q: %s", k, b)
		}
	}
	if _, ok := m["payload"]; ok {
		t.Fatal("empty payload must be omitted from the wire shape")
	}
}
