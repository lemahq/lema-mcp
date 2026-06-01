package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

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

	var wrote []string
	steps := []struct {
		label string
		fn    func(string) (bool, error)
		path  string
	}{
		{".mcp.json (registered lema-mcp server)", ensureMCPJSON, filepath.Join(dir, ".mcp.json")},
		{"AGENTS.md (decision-capture protocol)", ensureAgentsBlock, filepath.Join(dir, "AGENTS.md")},
		{".claude/settings.json (commit reminder hook)", ensureClaudeHook, filepath.Join(dir, ".claude", "settings.json")},
	}
	for _, st := range steps {
		changed, err := st.fn(st.path)
		if err != nil {
			return err
		}
		if changed {
			wrote = append(wrote, st.label)
		}
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
	return nil
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
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
