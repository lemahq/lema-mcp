package main

import (
	"bytes"
	"net/http"
	"os"
	"strings"
	"testing"
)

// The collector's boundary sync and SessionStart injection are fail-open by
// design: every failure is a silent local-only outcome so a hook is never
// blocked. That is correct behavior and must not change. It is also why the
// two paths carrying the relay are undiagnosable — missing credentials, a
// dark-flag 404, a run-identity mismatch and a timeout are indistinguishable
// no-ops.
//
// These tests pin the breadcrumb's contract: opt-in, cause-naming, and
// stderr-only. stdout is the SessionStart additionalContext channel — a stray
// byte there corrupts the injection itself, so no diagnostic may ever reach it.

func captureCollectorDebug(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	original := collectorDebugOut
	collectorDebugOut = &buf
	t.Cleanup(func() { collectorDebugOut = original })
	return &buf
}

// clearSyncEnv removes the hosted write config so the syncer cannot be built.
// Clearing the environment alone is not enough: resolveHostedWriteConfig falls
// back to ~/.config/lema/credentials, so a developer with real credentials
// takes a different branch than CI does. Pointing HOME at an empty dir makes
// the unconfigured case deterministic everywhere.
func clearSyncEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")
	t.Setenv("LEMA_WORKSPACE_ID", "")
	t.Setenv("HOME", t.TempDir())
}

func TestCollectorDebugSilentUnlessEnabled(t *testing.T) {
	buf := captureCollectorDebug(t)
	clearSyncEnv(t)
	t.Setenv("LEMA_COLLECTOR_DEBUG", "")

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-quiet")
	syncOnBoundary(dir, "claude-code", mkEnv("sess-quiet", "stop", nil))

	if buf.Len() != 0 {
		t.Fatalf("breadcrumb must be opt-in; got %q", buf.String())
	}
}

func TestCollectorDebugNamesUnconfiguredTarget(t *testing.T) {
	buf := captureCollectorDebug(t)
	clearSyncEnv(t)
	t.Setenv("LEMA_COLLECTOR_DEBUG", "1")

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-noconfig")
	syncOnBoundary(dir, "claude-code", mkEnv("sess-noconfig", "stop", nil))

	if !strings.Contains(buf.String(), "not configured") {
		t.Fatalf("want the missing-credentials cause named, got %q", buf.String())
	}
}

// The two causes share one `if` in syncOnBoundary today, so a checkpoint that
// is absent and a checkpoint that belongs to another run look identical. They
// are different problems: the first means the distiller never ran, the second
// means run identity drifted.
func TestCollectorDebugDistinguishesRunMismatchFromMissingCheckpoint(t *testing.T) {
	clearSyncEnv(t)
	t.Setenv("LEMA_COLLECTOR_DEBUG", "1")

	missing := captureCollectorDebug(t)
	syncOnBoundary(t.TempDir(), "claude-code", mkEnv("sess-absent", "stop", nil))
	missingMsg := missing.String()

	mismatch := captureCollectorDebug(t)
	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-owner")
	syncOnBoundary(dir, "claude-code", mkEnv("sess-other", "stop", nil))
	mismatchMsg := mismatch.String()

	if missingMsg == "" || mismatchMsg == "" {
		t.Fatalf("both causes must be reported; missing=%q mismatch=%q", missingMsg, mismatchMsg)
	}
	if missingMsg == mismatchMsg {
		t.Fatalf("absent checkpoint and run mismatch must not report identically: %q", missingMsg)
	}
	if !strings.Contains(mismatchMsg, "sess-owner") || !strings.Contains(mismatchMsg, "sess-other") {
		t.Fatalf("run mismatch must name both run ids, got %q", mismatchMsg)
	}
}

// syncOnBoundary discards syncCheckpoint's error entirely (`_ = ...`), so a
// 500, a 404 from a dark lema-run-state, a 401 and a timeout are one silence.
func TestCollectorDebugNamesSyncFailure(t *testing.T) {
	buf := captureCollectorDebug(t)
	srv := newSyncTestServer(t, &syncCapture{}, http.StatusInternalServerError)
	defer srv.Close()
	setSyncEnv(t, srv.URL)
	t.Setenv("LEMA_COLLECTOR_DEBUG", "1")

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-fail")
	syncOnBoundary(dir, "claude-code", mkEnv("sess-fail", "stop", nil))

	out := buf.String()
	if !strings.Contains(out, "500") {
		t.Fatalf("want the discarded transport error surfaced, got %q", out)
	}
}

// The demo-killer: when the hosted brief cannot be read, injectOnStart falls
// back to formatCheckpointBlock — the exact pre-0.21.4 output. Without a
// breadcrumb the operator sees a plausible block and cannot tell the relay
// never ran.
func TestInjectOnStartAnnouncesHostedBriefFallback(t *testing.T) {
	buf := captureCollectorDebug(t)
	clearSyncEnv(t)
	t.Setenv("LEMA_COLLECTOR_DEBUG", "1")

	original := collectorSyncerForCheckpoint
	collectorSyncerForCheckpoint = func(string) *collectorSyncer { return nil }
	t.Cleanup(func() { collectorSyncerForCheckpoint = original })

	dir := t.TempDir()
	writeTestCheckpoint(t, dir, "sess-fallback")
	injectOnStart(dir, mkEnv("sess-new", "session_start", nil))

	if !strings.Contains(buf.String(), "fell back") {
		t.Fatalf("want the silent degrade to the local block announced, got %q", buf.String())
	}
}

// stdout carries the SessionStart additionalContext JSON. A diagnostic byte
// there corrupts the very injection this breadcrumb exists to debug.
func TestCollectorDebugNeverWritesToStdout(t *testing.T) {
	clearSyncEnv(t)
	t.Setenv("LEMA_COLLECTOR_DEBUG", "1")

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = w
	// No checkpoint on disk: injectOnStart must emit no additionalContext,
	// so anything on stdout can only be the diagnostic leaking.
	injectOnStart(t.TempDir(), mkEnv("sess-nostdout", "session_start", nil))
	syncOnBoundary(t.TempDir(), "claude-code", mkEnv("sess-nostdout", "stop", nil))
	os.Stdout = original
	_ = w.Close()

	var got bytes.Buffer
	_, _ = got.ReadFrom(r)
	if got.Len() != 0 {
		t.Fatalf("diagnostics must never reach stdout; got %q", got.String())
	}
}
