package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeFixtureSession writes one transcript file under a
// root/<project-dir>/<id>.jsonl layout (mirroring ~/.claude/projects) and
// returns the root. Each line is a raw JSON record string.
func writeFixtureSession(t *testing.T, projectDir, id string, lines []string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, projectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var body string
	for _, ln := range lines {
		body += ln + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return root
}

// TestGetSessionCwdIsWorkspaceRoot is the load-bearing test for session resume:
// resume must spawn `claude` in the session's ORIGINAL workspace root, so the
// detail must surface the SHORTEST cwd seen in the transcript (the agent cd's into
// subdirs, so a deeper cwd appearing first must NOT win). If this regresses, a
// resumed session launches in the wrong directory and claude cannot find the id.
func TestGetSessionCwdIsWorkspaceRoot(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	const wsRoot = "/work/proj"
	// Deeper cwd appears FIRST; the workspace root appears later and is shorter.
	root := writeFixtureSession(t, "-work-proj", id, []string{
		`{"type":"assistant","cwd":"/work/proj/internal/deep","gitBranch":"main","timestamp":"2026-06-02T10:01:00Z","message":{"content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"user","cwd":"/work/proj","gitBranch":"main","timestamp":"2026-06-02T10:00:00Z","message":{"content":"resume me"}}`,
	})

	src := NewLocalSessionSource(root)
	detail, err := src.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	if detail.Cwd != wsRoot {
		t.Errorf("detail.Cwd = %q, want workspace root %q (shortest cwd, not the deeper one seen first)", detail.Cwd, wsRoot)
	}
	if detail.Repo != filepath.Base(wsRoot) {
		t.Errorf("detail.Repo = %q, want %q", detail.Repo, filepath.Base(wsRoot))
	}
}

// TestSessionMetaHasNoCwd encodes the privacy intent: the full filesystem cwd is a
// DETAIL-only field. The list payload (SessionMeta) must never carry it, so a
// future org-wide HostedSessionSource does not broadcast every engineer's paths on
// the cheap list call.
func TestSessionMetaHasNoCwd(t *testing.T) {
	b, err := json.Marshal(SessionMeta{ID: "x"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["cwd"]; ok {
		t.Errorf("SessionMeta JSON includes a cwd key; cwd must be detail-only (SessionDetail), not on the list payload")
	}
}
