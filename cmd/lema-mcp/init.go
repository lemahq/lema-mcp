package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// configWriteMu serializes the read-modify-write cycle on the repo config files
// (.mcp.json, .claude/settings.json) so two rapid Plugins-panel toggles
// (ADR-0043/0044) cannot lose-update each other: each toggle reads, mutates, and
// writes, and without serialization the second writer would clobber the first
// toggle's change. The surface is a handful of files, so one process-wide lock is
// simpler and sufficient compared with a per-path lock. Callers that read+modify
// (toggleMCP, toggleLemaHook, ensure*/remove* via writeJSON) run while holding it.
var configWriteMu sync.Mutex

// runInit wires a repo for lema decision capture in one command (ADR-0042): it
// registers the MCP server (.mcp.json), writes the capture protocol that drives
// the agent (AGENTS.md), and installs a commit reminder hook
// (.claude/settings.json). Every step is non-destructive and idempotent —
// existing config is merged, not clobbered, and re-running changes nothing.
func runInit(args []string) error {
	dir := "."
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		dir = args[0]
	}

	wrote, err := initRepo(dir)
	if err != nil {
		return err
	}

	if len(wrote) == 0 {
		fmt.Println("lema-mcp init: already set up — nothing to change.")
		return nil
	}
	fmt.Println("lema-mcp init: decision capture is set up")
	for _, w := range wrote {
		fmt.Println("  + " + w)
	}
	fmt.Println("\nNext: open this repo in Claude Code, approve the lema-mcp server, and your")
	fmt.Println("agent will record decisions (and what it ruled out) as it works.")
	fmt.Println()
	fmt.Println("What you just turned on — never-reopen: when your agent reaches for a")
	fmt.Println("decision you already killed, it gets \"CLOSED — do not propose X\" and surfaces")
	fmt.Println("the prior decision instead. See it in 30 seconds:  npx lema-mcp demo")
	return nil
}

// initRepo runs the capture-setup steps for dir and returns the labels of what it
// wrote (empty if already set up). Shared by the `init` CLI command (runInit) and
// the serve --http POST /api/init endpoint (the GUI's "enable capture" button), so
// the GUI and the CLI register lema identically.
func initRepo(dir string) ([]string, error) {
	// Serialize against the Plugins-panel toggles, which mutate the same config
	// files, so a concurrent init and toggle cannot lose-update each other
	// (ADR-0043/0044). Held across all steps' read-modify-write.
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	var wrote []string
	steps := []struct {
		label string
		fn    func(string) (bool, error)
		path  string
	}{
		{".mcp.json (registered lema-mcp server)", ensureMCPJSON, filepath.Join(dir, ".mcp.json")},
		{"AGENTS.md (decision-capture protocol)", ensureAgentsBlock, filepath.Join(dir, "AGENTS.md")},
		{".claude/settings.json (commit reminder hook)", ensureClaudeHook, filepath.Join(dir, ".claude", "settings.json")},
		{".claude/settings.json (never-reopen guard hook)", ensurePreToolUseHook, filepath.Join(dir, ".claude", "settings.json")},
		{".claude/settings.json (capture-nudge hook)", ensureCaptureNudgeHook, filepath.Join(dir, ".claude", "settings.json")},
	}
	for _, st := range steps {
		changed, err := st.fn(st.path)
		if err != nil {
			return nil, err
		}
		if changed {
			wrote = append(wrote, st.label)
		}
	}
	return wrote, nil
}

// ensureMCPJSON registers the lema server in .mcp.json, preserving any servers
// already configured. Reports whether it changed the file.
func ensureMCPJSON(path string) (bool, error) {
	root, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if _, ok := servers["lema"]; ok {
		return false, nil
	}
	servers["lema"] = map[string]any{
		"command": "npx",
		"args":    []any{"-y", "lema-mcp@latest"},
	}
	root["mcpServers"] = servers
	return true, writeJSON(path, root)
}

const reminderMarker = "record it with record_decision"

// ensureClaudeHook adds a PostToolUse reminder that fires after a git commit,
// nudging the agent to record any decision it just settled. The command
// self-filters to git commits so it is silent on every other Bash call. Existing
// hooks and settings are preserved; idempotent via the reminder text.
func ensureClaudeHook(path string) (bool, error) {
	root, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	if b, err := json.Marshal(root); err == nil && strings.Contains(string(b), reminderMarker) {
		return false, nil
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	postToolUse, _ := hooks["PostToolUse"].([]any)
	reminder := map[string]any{
		"matcher": "Bash",
		"hooks": []any{map[string]any{
			"type": "command",
			"command": "grep -q 'git commit' 2>/dev/null && " +
				"echo 'lema: settled a decision? " + reminderMarker +
				" — the chosen option and what you rejected.' || true",
		}},
	}
	hooks["PostToolUse"] = append(postToolUse, reminder)
	root["hooks"] = hooks
	return true, writeJSON(path, root)
}

const guardMarker = "lema-mcp@latest guard"

// ensurePreToolUseHook installs the never-reopen guard: a PreToolUse hook that runs
// `lema-mcp guard` before every Edit/Write, surfacing a CLOSED decision the change
// would re-litigate (ADR-0052). Mirrors ensureClaudeHook — idempotent via
// guardMarker, preserves existing hooks.
func ensurePreToolUseHook(path string) (bool, error) {
	root, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	if b, err := json.Marshal(root); err == nil && strings.Contains(string(b), guardMarker) {
		return false, nil
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	preToolUse, _ := hooks["PreToolUse"].([]any)
	guard := map[string]any{
		"matcher": "Edit|Write",
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": "npx -y lema-mcp@latest guard",
		}},
	}
	hooks["PreToolUse"] = append(preToolUse, guard)
	root["hooks"] = hooks
	return true, writeJSON(path, root)
}

const captureNudgeMarker = "lema-mcp@latest nudge"

// ensureCaptureNudgeHook installs the capture nudge: a PostToolUse hook that runs
// `lema-mcp nudge` after Edit/Write/MultiEdit and reminds the agent to
// record_decision when it edits a dependency manifest (ADR-0054). Additive to the
// commit reminder; idempotent via captureNudgeMarker; preserves existing hooks.
func ensureCaptureNudgeHook(path string) (bool, error) {
	root, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	if b, err := json.Marshal(root); err == nil && strings.Contains(string(b), captureNudgeMarker) {
		return false, nil
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	postToolUse, _ := hooks["PostToolUse"].([]any)
	nudge := map[string]any{
		"matcher": "Edit|Write|MultiEdit",
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": "npx -y lema-mcp@latest nudge",
		}},
	}
	hooks["PostToolUse"] = append(postToolUse, nudge)
	root["hooks"] = hooks
	return true, writeJSON(path, root)
}

// removeCaptureNudgeHook deletes ONLY the capture-nudge PostToolUse entry (whose
// command contains captureNudgeMarker), leaving the commit reminder and every other
// hook untouched — the inverse of ensureCaptureNudgeHook for the Plugins panel "off"
// (ADR-0054). Empty groups and the PostToolUse/hooks keys are pruned when they go
// empty. Reports whether it changed the file.
func removeCaptureNudgeHook(path string) (bool, error) {
	root, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	hooks, _ := root["hooks"].(map[string]any)
	groups, _ := hooks["PostToolUse"].([]any)
	if len(groups) == 0 {
		return false, nil
	}

	keptGroups := make([]any, 0, len(groups))
	changed := false
	for _, g := range groups {
		group, _ := g.(map[string]any)
		entries, _ := group["hooks"].([]any)
		keptEntries := make([]any, 0, len(entries))
		for _, h := range entries {
			entry, _ := h.(map[string]any)
			command, _ := entry["command"].(string)
			if strings.Contains(command, captureNudgeMarker) {
				changed = true
				continue // drop the lema-managed capture-nudge hook
			}
			keptEntries = append(keptEntries, h)
		}
		if len(keptEntries) == 0 {
			continue
		}
		group["hooks"] = keptEntries
		keptGroups = append(keptGroups, group)
	}
	if !changed {
		return false, nil
	}

	if len(keptGroups) == 0 {
		delete(hooks, "PostToolUse")
	} else {
		hooks["PostToolUse"] = keptGroups
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}
	return true, writeJSON(path, root)
}

// disableMcpServer adds name to the disabledMcpjsonServers array in the repo
// settings.json without removing the server from .mcp.json — the non-destructive
// "off" used by the Plugins panel (ADR-0043/0044). Idempotent: a name already
// listed leaves the file unchanged. Creates the key (and file) if absent.
func disableMcpServer(path, name string) error {
	root, err := readJSONObject(path)
	if err != nil {
		return err
	}
	current, _ := root["disabledMcpjsonServers"].([]any)
	for _, v := range current {
		if s, ok := v.(string); ok && s == name {
			return nil // already disabled
		}
	}
	root["disabledMcpjsonServers"] = append(current, name)
	return writeJSON(path, root)
}

// enableMcpServer removes name from disabledMcpjsonServers in the repo
// settings.json — the inverse of disableMcpServer. Idempotent and
// non-destructive: it never touches .mcp.json, and drops the key entirely once
// the last disabled name is removed so the file stays clean. A missing key or
// file is a no-op.
func enableMcpServer(path, name string) error {
	root, err := readJSONObject(path)
	if err != nil {
		return err
	}
	current, ok := root["disabledMcpjsonServers"].([]any)
	if !ok || len(current) == 0 {
		return nil
	}
	kept := make([]any, 0, len(current))
	changed := false
	for _, v := range current {
		if s, ok := v.(string); ok && s == name {
			changed = true
			continue
		}
		kept = append(kept, v)
	}
	if !changed {
		return nil
	}
	if len(kept) == 0 {
		delete(root, "disabledMcpjsonServers")
	} else {
		root["disabledMcpjsonServers"] = kept
	}
	return writeJSON(path, root)
}

// removeLemaHook deletes ONLY the lema commit-reminder hook (the PostToolUse
// entry whose command contains reminderMarker) from the repo settings.json,
// leaving every other hook untouched — the non-destructive inverse of
// ensureClaudeHook used by the Plugins panel (ADR-0043/0044). Empty hook groups
// and the PostToolUse/hooks keys are pruned when they go empty so the file does
// not accumulate dead structure. Reports whether it changed the file.
func removeLemaHook(path string) (bool, error) {
	root, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	hooks, _ := root["hooks"].(map[string]any)
	groups, _ := hooks["PostToolUse"].([]any)
	if len(groups) == 0 {
		return false, nil
	}

	keptGroups := make([]any, 0, len(groups))
	changed := false
	for _, g := range groups {
		group, _ := g.(map[string]any)
		entries, _ := group["hooks"].([]any)
		keptEntries := make([]any, 0, len(entries))
		for _, h := range entries {
			entry, _ := h.(map[string]any)
			command, _ := entry["command"].(string)
			if strings.Contains(command, reminderMarker) {
				changed = true
				continue // drop the lema-managed hook
			}
			keptEntries = append(keptEntries, h)
		}
		if len(keptEntries) == 0 {
			// Every entry in this group was the lema hook; drop the now-empty group.
			continue
		}
		group["hooks"] = keptEntries
		keptGroups = append(keptGroups, group)
	}
	if !changed {
		return false, nil
	}

	if len(keptGroups) == 0 {
		delete(hooks, "PostToolUse")
	} else {
		hooks["PostToolUse"] = keptGroups
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}
	return true, writeJSON(path, root)
}

// removeGuardHook deletes ONLY the lema never-reopen guard (the PreToolUse entry
// whose command contains guardMarker), leaving every other hook untouched — the
// inverse of ensurePreToolUseHook for the Plugins panel "off" (ADR-0043/0044,
// ADR-0052). Empty groups and the PreToolUse/hooks keys are pruned when they go
// empty. Reports whether it changed the file.
func removeGuardHook(path string) (bool, error) {
	root, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	hooks, _ := root["hooks"].(map[string]any)
	groups, _ := hooks["PreToolUse"].([]any)
	if len(groups) == 0 {
		return false, nil
	}

	keptGroups := make([]any, 0, len(groups))
	changed := false
	for _, g := range groups {
		group, _ := g.(map[string]any)
		entries, _ := group["hooks"].([]any)
		keptEntries := make([]any, 0, len(entries))
		for _, h := range entries {
			entry, _ := h.(map[string]any)
			command, _ := entry["command"].(string)
			if strings.Contains(command, guardMarker) {
				changed = true
				continue // drop the lema-managed guard hook
			}
			keptEntries = append(keptEntries, h)
		}
		if len(keptEntries) == 0 {
			continue
		}
		group["hooks"] = keptEntries
		keptGroups = append(keptGroups, group)
	}
	if !changed {
		return false, nil
	}

	if len(keptGroups) == 0 {
		delete(hooks, "PreToolUse")
	} else {
		hooks["PreToolUse"] = keptGroups
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}
	return true, writeJSON(path, root)
}

const (
	captureBlockStart = "<!-- lema:capture:start -->"
	captureBlockEnd   = "<!-- lema:capture:end -->"
)

// captureProtocol is the instruction that actually drives capture: it tells the
// agent when to record a decision and to check before proposing. It is the real
// mechanism — the hook only reminds (a Claude Code hook runs after the model
// stops and cannot form the decision itself; see ADR-0042).
const captureProtocol = `## lema decision capture

This repo runs the **lema** MCP server. Keep decisions durable and avoid
re-litigating settled ones:

- **When you settle a non-trivial decision** (a library, a pattern, an
  architecture or policy choice — not a rename or a bug fix), call ` + "`record_decision`" + `
  with the option you ` + "`chose`" + ` **and** the ` + "`rejected`" + ` alternatives and *why* each was
  killed. The rejected alternatives are the point — they stop this decision from
  being reopened.
- **Before you propose a direction** (a library, an approach, a design), call
  ` + "`check_decided`" + ` first. If anything comes back CLOSED, do not re-propose it —
  surface the prior decision instead.
- Treat a CLOSED result from ` + "`search_decisions`" + ` or ` + "`check_decided`" + ` as binding: that
  option was already ruled out or superseded.`

// ensureAgentsBlock writes the capture protocol into AGENTS.md as a managed
// block, replacing an earlier copy in place so re-running is idempotent and
// never disturbs the rest of the file.
func ensureAgentsBlock(path string) (bool, error) {
	block := captureBlockStart + "\n" + captureProtocol + "\n" + captureBlockEnd

	var existing string
	if data, err := os.ReadFile(path); err == nil {
		existing = string(data)
	} else if !os.IsNotExist(err) {
		return false, err
	}

	if i := strings.Index(existing, captureBlockStart); i >= 0 {
		j := strings.Index(existing, captureBlockEnd)
		if j > i {
			updated := existing[:i] + block + existing[j+len(captureBlockEnd):]
			if updated == existing {
				return false, nil
			}
			return true, os.WriteFile(path, []byte(updated), 0o644)
		}
	}

	var sb strings.Builder
	sb.WriteString(existing)
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		sb.WriteString("\n")
	}
	if existing != "" {
		sb.WriteString("\n")
	}
	sb.WriteString(block)
	sb.WriteString("\n")
	return true, os.WriteFile(path, []byte(sb.String()), 0o644)
}

// readJSONObject reads a JSON object file into a map, treating a missing file as
// an empty object and a malformed file as an error (so init never silently
// discards a config it could not parse).
func readJSONObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	root := map[string]any{}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return nil, fmt.Errorf("%s exists but is not valid JSON: %w", path, err)
		}
	}
	return root, nil
}

// writeJSON writes v as pretty JSON, creating the parent directory if needed.
// HTML escaping is disabled so shell operators in hook commands (>, &) stay
// readable in the file rather than rendering as > / &.
//
// The write is atomic: v is encoded into a sibling temp file and then renamed
// into place, so a crash mid-write can never leave a truncated, unparseable
// config that would brick the Plugins panel on the next read (ADR-0043/0044).
// os.Rename is atomic within a filesystem on macOS/Linux, and the temp file lives
// in the same directory as path so the rename never crosses a filesystem.
func writeJSON(path string, v any) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp) // best-effort cleanup; the original file is untouched
		return err
	}
	return nil
}
