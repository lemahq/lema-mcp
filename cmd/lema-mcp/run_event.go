// run_event.go is the `lema-mcp run-event` subcommand — run-ledger v1 local slice.
// Hooks append structured events to a per-tab JSONL spool; PreCompact distills a
// checkpoint; SessionStart injects it as additionalContext. Dark unless
// LEMA_RUN_LEDGER=1 in the hook process env (set per-PTY by lema-terminal).
// Fail-open everywhere; always exit 0.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	runLedgerEnv     = "LEMA_RUN_LEDGER"
	runTabEnv        = "LEMA_RUN_TAB_ID"
	runSpoolDirEnv   = "LEMA_RUN_SPOOL_DIR"
	runMaxPromptKeep = 3
	runMaxFileKeep   = 8
)

// runEventInput is the union of Claude Code hook stdin fields run-event reads.
type runEventInput struct {
	SessionID string         `json:"session_id"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	Prompt    string         `json:"prompt"`
}

type runSpoolEvent struct {
	At                string `json:"at"`
	TabID             string `json:"tab_id"`
	PhysicalSessionID string `json:"physical_session_id,omitempty"`
	Kind              string `json:"kind"`
	ToolName          string `json:"tool_name,omitempty"`
	FilePath          string `json:"file_path,omitempty"`
	Prompt            string `json:"prompt,omitempty"`
}

type runCheckpoint struct {
	TabID             string   `json:"tab_id"`
	LogicalRunID      string   `json:"logical_run_id"`
	PhysicalSessionID string   `json:"physical_session_id,omitempty"`
	UpdatedAt         string   `json:"updated_at"`
	Summary           string   `json:"summary"`
	RecentPrompts     []string `json:"recent_prompts"`
	FilesTouched      []string `json:"files_touched"`
	EventCount        int      `json:"event_count"`
}

func runEventEnabled() bool {
	v := strings.TrimSpace(os.Getenv(runLedgerEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

func runEventTabID() string {
	if t := strings.TrimSpace(os.Getenv(runTabEnv)); t != "" {
		return sanitizeTabID(t)
	}
	return "default"
}

func sanitizeTabID(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "default"
	}
	return out
}

func runEventSpoolDir() (string, error) {
	if d := strings.TrimSpace(os.Getenv(runSpoolDirEnv)); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lema", "run-spool"), nil
}

func spoolPath(dir, tabID string) string {
	return filepath.Join(dir, tabID+".jsonl")
}

func checkpointPath(dir, tabID string) string {
	return filepath.Join(dir, tabID+".checkpoint.json")
}

func appendSpoolEvent(dir, tabID string, ev runSpoolEvent) (err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(spoolPath(dir, tabID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
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

func readSpoolEvents(dir, tabID string) ([]runSpoolEvent, error) {
	f, err := os.Open(spoolPath(dir, tabID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []runSpoolEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev runSpoolEvent
		if json.Unmarshal(sc.Bytes(), &ev) == nil {
			out = append(out, ev)
		}
	}
	return out, sc.Err()
}

func toolFilePath(in runEventInput) string {
	if in.ToolInput == nil {
		return ""
	}
	p, _ := in.ToolInput["file_path"].(string)
	return strings.TrimSpace(p)
}

func distillCheckpoint(events []runSpoolEvent, tabID string) runCheckpoint {
	cp := runCheckpoint{
		TabID:        tabID,
		LogicalRunID: tabID,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
		EventCount:   len(events),
	}
	seenFiles := map[string]bool{}
	var prompts []string
	for i := len(events) - 1; i >= 0 && len(prompts) < runMaxPromptKeep; i-- {
		ev := events[i]
		if ev.Kind == "user_prompt" && strings.TrimSpace(ev.Prompt) != "" {
			prompts = append([]string{strings.TrimSpace(ev.Prompt)}, prompts...)
		}
	}
	for _, ev := range events {
		if ev.PhysicalSessionID != "" && cp.PhysicalSessionID == "" {
			cp.PhysicalSessionID = ev.PhysicalSessionID
		}
		if ev.Kind == "tool_use" && ev.FilePath != "" && !seenFiles[ev.FilePath] {
			seenFiles[ev.FilePath] = true
			cp.FilesTouched = append(cp.FilesTouched, ev.FilePath)
		}
	}
	if len(cp.FilesTouched) > runMaxFileKeep {
		cp.FilesTouched = cp.FilesTouched[len(cp.FilesTouched)-runMaxFileKeep:]
	}
	cp.RecentPrompts = prompts
	cp.Summary = buildCheckpointSummary(cp)
	return cp
}

func buildCheckpointSummary(cp runCheckpoint) string {
	var parts []string
	if n := len(cp.RecentPrompts); n > 0 {
		last := cp.RecentPrompts[n-1]
		if len([]rune(last)) > 120 {
			last = string([]rune(last)[:120]) + "…"
		}
		parts = append(parts, "last prompt: "+last)
	}
	if len(cp.FilesTouched) > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s) touched", len(cp.FilesTouched)))
	}
	if cp.EventCount > 0 {
		parts = append(parts, fmt.Sprintf("%d events spooled", cp.EventCount))
	}
	if len(parts) == 0 {
		return "run started — no activity captured yet"
	}
	return strings.Join(parts, "; ")
}

func writeCheckpoint(dir string, cp runCheckpoint) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(checkpointPath(dir, cp.TabID), b, 0o600)
}

func readCheckpoint(dir, tabID string) (runCheckpoint, bool) {
	b, err := os.ReadFile(checkpointPath(dir, tabID))
	if err != nil {
		return runCheckpoint{}, false
	}
	var cp runCheckpoint
	if json.Unmarshal(b, &cp) != nil {
		return runCheckpoint{}, false
	}
	return cp, true
}

func formatInjectBlock(cp runCheckpoint) string {
	var b strings.Builder
	b.WriteString("lema run-ledger checkpoint (logical run ")
	b.WriteString(cp.LogicalRunID)
	b.WriteString("):\n")
	b.WriteString(cp.Summary)
	b.WriteString("\n")
	if len(cp.RecentPrompts) > 0 {
		b.WriteString("\nRecent prompts:\n")
		for i, p := range cp.RecentPrompts {
			b.WriteString(fmt.Sprintf("%d. %s\n", i+1, p))
		}
	}
	if len(cp.FilesTouched) > 0 {
		b.WriteString("\nFiles touched:\n")
		for _, f := range cp.FilesTouched {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nTreat this as continuity from prior physical sessions — do not re-litigate settled work unless the user asks.")
	return b.String()
}

func emitAdditionalContext(hookEvent, msg string) {
	if strings.TrimSpace(msg) == "" {
		return
	}
	out := guardOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName:     hookEvent,
		AdditionalContext: msg,
	}}
	if b, err := json.Marshal(out); err == nil {
		fmt.Println(string(b))
	}
}

func runRunEvent(args []string) {
	if !runEventEnabled() {
		return
	}
	hookEvent := "SessionStart"
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		hookEvent = strings.TrimSpace(args[0])
	}
	dir, err := runEventSpoolDir()
	if err != nil {
		return
	}
	tabID := runEventTabID()

	switch hookEvent {
	case "SessionStart":
		if cp, ok := readCheckpoint(dir, tabID); ok && cp.Summary != "" {
			emitAdditionalContext("SessionStart", formatInjectBlock(cp))
		}
		_ = appendSpoolEvent(dir, tabID, runSpoolEvent{
			At:    time.Now().UTC().Format(time.RFC3339),
			TabID: tabID,
			Kind:  "session_start",
		})
	case "PreCompact":
		events, err := readSpoolEvents(dir, tabID)
		if err != nil {
			return
		}
		cp := distillCheckpoint(events, tabID)
		_ = writeCheckpoint(dir, cp)
		_ = appendSpoolEvent(dir, tabID, runSpoolEvent{
			At:    time.Now().UTC().Format(time.RFC3339),
			TabID: tabID,
			Kind:  "pre_compact",
		})
	default:
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return
		}
		var in runEventInput
		if err := json.Unmarshal(data, &in); err != nil {
			return
		}
		ev := runSpoolEvent{
			At:                time.Now().UTC().Format(time.RFC3339),
			TabID:             tabID,
			PhysicalSessionID: strings.TrimSpace(in.SessionID),
			Kind:              hookKindFromEvent(hookEvent),
			ToolName:          in.ToolName,
			FilePath:          toolFilePath(in),
			Prompt:            strings.TrimSpace(in.Prompt),
		}
		_ = appendSpoolEvent(dir, tabID, ev)
	}
}

func hookKindFromEvent(hookEvent string) string {
	switch hookEvent {
	case "UserPromptSubmit":
		return "user_prompt"
	case "PostToolUse", "PreToolUse":
		return "tool_use"
	default:
		return strings.ToLower(hookEvent)
	}
}
