package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// manifestFiles are the dependency manifests whose edit signals a likely
// library/framework decision worth recording (ADR-0054). It is an allowlist, so
// generated lock files (package-lock.json, go.sum, Cargo.lock, …) stay silent.
var manifestFiles = map[string]bool{
	"go.mod":           true,
	"package.json":     true,
	"cargo.toml":       true,
	"pyproject.toml":   true,
	"requirements.txt": true,
	"gemfile":          true,
	"build.gradle":     true,
	"pom.xml":          true,
}

// nudgeReminder returns the capture reminder to surface for a tool call, or "" when
// the call is not a decision-shaped moment. v1 fires only on an Edit/Write/MultiEdit
// to a dependency manifest — the canonical record_decision moment — and stays silent
// otherwise so it never becomes naggy (ADR-0054).
func nudgeReminder(in guardInput) string {
	switch in.ToolName {
	case "Edit", "Write", "MultiEdit":
	default:
		return ""
	}
	p, _ := in.ToolInput["file_path"].(string)
	if p == "" || !manifestFiles[strings.ToLower(filepath.Base(p))] {
		return ""
	}
	return "lema: you changed dependencies in " + filepath.Base(p) +
		". If you chose a library or framework, call record_decision with the option you chose AND the alternatives you rejected — so the team and future agents don't re-litigate it."
}

// runNudge is the `lema-mcp nudge` subcommand — the PostToolUse capture nudge. It
// reads the hook payload from stdin and, on a decision-shaped edit, emits a
// non-blocking additionalContext reminder to record the decision. Capture stays
// instruction-driven: the nudge reminds, the agent forms the atom (ADR-0042/0054).
// Fail-open; always exit 0 and emit nothing on any error.
func runNudge(_ []string) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	var in guardInput
	if err := json.Unmarshal(data, &in); err != nil {
		return
	}
	msg := nudgeReminder(in)
	if msg == "" {
		return
	}
	out := guardOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName:     "PostToolUse",
		AdditionalContext: msg,
	}}
	if b, err := json.Marshal(out); err == nil {
		fmt.Println(string(b))
	}
}
