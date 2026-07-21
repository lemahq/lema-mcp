// collector.go is the `lema-mcp collect` subcommand — the F3 open collector
// (pivot B2). Harness adapters normalize harness-native events into the
// run-identity envelope {run_id, ts, kind, payload, evidence}; envelopes append
// to a per-run local JSONL spool with expiring semantics. Run identity comes
// from the ADAPTER (e.g. Claude Code's session_id) — never LEMA_RUN_TAB_ID,
// which belongs to the paused run-event/lema-terminal mechanism. No env gate:
// collecting is default behavior. Fail-open everywhere; always exit 0 — a hook
// must never block the harness. The only stdout is the F4 SessionStart
// checkpoint injection (collector_checkpoint.go); every other path is silent.
//
// Hosted sync (envelopes → the runs/run_events API when credentials exist) is
// a named follow-up riding the same envelope; v1 spools locally only.
package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	collectorDirEnv = "LEMA_COLLECTOR_DIR"
	// collectorTTL is the local spool's expiry horizon. Active-work state is
	// expiring by design (mirror of the server-side run_events posture, D6):
	// a run file untouched this long is pruned opportunistically on append.
	collectorTTL = 14 * 24 * time.Hour
)

// collectorEnvelope is the F3 run-identity envelope. Payload holds the
// harness-normalized fields; Evidence points back at the raw source (harness,
// hook event, transcript path) so nothing in the spool is unattributable.
type collectorEnvelope struct {
	RunID    string            `json:"run_id"`
	TS       string            `json:"ts"`
	Kind     string            `json:"kind"`
	Payload  map[string]string `json:"payload,omitempty"`
	Evidence map[string]string `json:"evidence,omitempty"`
}

// harnessAdapter normalizes one harness-native event into an envelope.
// ok=false means "nothing to collect" (unidentifiable or empty) — the
// collector skips silently; adapters never fabricate run identity.
type harnessAdapter interface {
	name() string
	normalize(hookEvent string, stdin []byte) (collectorEnvelope, bool)
}

// claudeCodeAdapter reads Claude Code hook stdin JSON. Run identity is the
// harness session_id; an event without one is skipped, not guessed.
type claudeCodeAdapter struct{}

// claudeHookInput is the union of Claude Code hook stdin fields the adapter reads.
type claudeHookInput struct {
	SessionID      string         `json:"session_id"`
	TranscriptPath string         `json:"transcript_path"`
	CWD            string         `json:"cwd"`
	ToolName       string         `json:"tool_name"`
	ToolInput      map[string]any `json:"tool_input"`
	Prompt         string         `json:"prompt"`
}

func (claudeCodeAdapter) name() string { return "claude-code" }

func (a claudeCodeAdapter) normalize(hookEvent string, stdin []byte) (collectorEnvelope, bool) {
	var in claudeHookInput
	if err := json.Unmarshal(stdin, &in); err != nil {
		return collectorEnvelope{}, false
	}
	runID := strings.TrimSpace(in.SessionID)
	if runID == "" {
		return collectorEnvelope{}, false
	}
	ev := collectorEnvelope{
		RunID: runID,
		TS:    time.Now().UTC().Format(time.RFC3339),
		Kind:  collectorKind(hookEvent),
		Evidence: map[string]string{
			"harness":    a.name(),
			"hook_event": hookEvent,
		},
	}
	if p := strings.TrimSpace(in.TranscriptPath); p != "" {
		ev.Evidence["transcript_path"] = p
	}
	if c := strings.TrimSpace(in.CWD); c != "" {
		ev.Evidence["cwd"] = c
	}
	payload := map[string]string{}
	if t := strings.TrimSpace(in.ToolName); t != "" {
		payload["tool_name"] = t
	}
	if in.ToolInput != nil {
		if fp, _ := in.ToolInput["file_path"].(string); strings.TrimSpace(fp) != "" {
			payload["file_path"] = strings.TrimSpace(fp)
		}
	}
	if pr := strings.TrimSpace(in.Prompt); pr != "" {
		payload["prompt"] = pr
	}
	if len(payload) > 0 {
		ev.Payload = payload
	}
	return ev, true
}

// collectorKind maps a harness hook-event name onto the envelope's kind
// vocabulary (same mapping run-event used, plus the lifecycle tails).
func collectorKind(hookEvent string) string {
	switch hookEvent {
	case "SessionStart":
		return "session_start"
	case "UserPromptSubmit":
		return "user_prompt"
	case "PreToolUse", "PostToolUse":
		return "tool_use"
	case "Stop":
		return "stop"
	case "PreCompact":
		return "pre_compact"
	case "SessionEnd":
		return "session_end"
	default:
		return strings.ToLower(strings.TrimSpace(hookEvent))
	}
}

// collectorAdapterFor returns the adapter for a harness name, or nil for a
// harness this build does not know (the caller skips — fail-open). Codex is
// the named next adapter; it ships when its event source is implemented for
// real, not as a stub.
func collectorAdapterFor(harness string) harnessAdapter {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "claude-code", "claudecode", "claude":
		return claudeCodeAdapter{}
	default:
		return nil
	}
}

func collectorDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv(collectorDirEnv)); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lema", "runs"), nil
}

func collectorRunPath(dir, runID string) string {
	return filepath.Join(dir, sanitizeTabID(runID)+".jsonl")
}

// appendEnvelope appends one envelope to the run's spool file (0600, dir 0700).
func appendEnvelope(dir string, ev collectorEnvelope) (err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := collectorRunPath(dir, ev.RunID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// Freshen mtime immediately: a run resuming after >TTL dormancy would
	// otherwise present a stale mtime while we hold the fd, and a concurrent
	// process's prune pass could unlink the file mid-append (write lost on an
	// unlinked inode). The bump closes that window to the open→chtimes gap.
	now := time.Now()
	_ = os.Chtimes(path, now, now)
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// pruneExpiredRuns removes run spool files whose last write is older than the
// TTL. Best-effort: any error just leaves the file for a later pass.
func pruneExpiredRuns(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > collectorTTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// readRunEnvelopes loads a run's spooled envelopes (missing file = empty run).
// Consumed by the checkpoint/handoff half of B2 (F4).
func readRunEnvelopes(dir, runID string) ([]collectorEnvelope, error) {
	b, err := os.ReadFile(collectorRunPath(dir, runID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []collectorEnvelope
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev collectorEnvelope
		if json.Unmarshal([]byte(line), &ev) == nil && ev.RunID != "" {
			out = append(out, ev)
		}
	}
	return out, nil
}

// runCollect is the `lema-mcp collect <harness> <hook-event>` entrypoint.
// Reads one harness event from stdin, normalizes it through the adapter, and
// appends it to the per-run spool. Silent and exit-0 on every failure path.
func runCollect(args []string) {
	if len(args) < 2 {
		return
	}
	adapter := collectorAdapterFor(args[0])
	if adapter == nil {
		return
	}
	hookEvent := strings.TrimSpace(args[1])
	if hookEvent == "" {
		return
	}
	stdin, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		return
	}
	ev, ok := adapter.normalize(hookEvent, stdin)
	if !ok {
		return
	}
	dir, err := collectorDir()
	if err != nil {
		return
	}
	// Prune BEFORE appending so this process never removes the file it is
	// about to write (a dormant run's expired spool is pruned here, then the
	// new envelope starts a fresh file).
	pruneExpiredRuns(dir, time.Now())
	// F4: a run boundary distills the pre-boundary spool into the project
	// checkpoint (the lifecycle marker itself is not "activity" — same order
	// run_event.go uses); a session start injects the prior checkpoint (the
	// collector's only stdout, and only here).
	checkpointOnBoundary(dir, ev)
	if err := appendEnvelope(dir, ev); err != nil {
		return
	}
	injectOnStart(dir, ev)
}
