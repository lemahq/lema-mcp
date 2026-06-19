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

// manifestExts are file extensions (lowercased) that signal an infra decision
// moment. Unlike dependency manifests, Terraform files never have unique
// basenames (every module has a main.tf), so extension-based matching is used
// instead of the basename map. .tfstate and .hcl are excluded: .tfstate is a
// generated artifact; .terraform.lock.hcl is a lock file.
var manifestExts = map[string]bool{
	".tf":     true,
	".tfvars": true,
}

// isManifestDecisionEdit is the shared decision-moment classifier: an
// Edit/Write/MultiEdit touching a dependency manifest or an infra file. It is
// used by BOTH the capture nudge (to remind) and the capture-rate gauge (to
// count the denominator), so the gauge measures exactly what the nudge
// classifies and the two can never drift apart.
func isManifestDecisionEdit(toolName, filePath string) bool {
	switch toolName {
	case "Edit", "Write", "MultiEdit":
	default:
		return false
	}
	if filePath == "" {
		return false
	}
	// Extension-based check for Terraform files (basenames are not unique).
	if manifestExts[strings.ToLower(filepath.Ext(filePath))] {
		return true
	}
	return manifestFiles[strings.ToLower(filepath.Base(filePath))]
}

// isTerraformFile reports whether the path has a Terraform extension (.tf or
// .tfvars). Single source of truth for the infra-vs-dependency branch so that
// adding a new extension to manifestExts only requires one additional site.
func isTerraformFile(filePath string) bool {
	return manifestExts[strings.ToLower(filepath.Ext(filePath))]
}

// nudgeReminder returns the capture reminder to surface for a tool call, or "" when
// the call is not a decision-shaped moment. v1 fires only on an Edit/Write/MultiEdit
// to a dependency manifest or infra file — the canonical record_decision moment —
// and stays silent otherwise so it never becomes naggy (ADR-0054).
func nudgeReminder(in guardInput) string {
	p, _ := in.ToolInput["file_path"].(string)
	if !isManifestDecisionEdit(in.ToolName, p) {
		return ""
	}
	if isTerraformFile(p) {
		return "lema: you changed infra in " + filepath.Base(p) +
			". If you chose a provider, module, or resource configuration, call record_decision with the option you chose AND the alternatives you rejected — so the team and future agents don't re-litigate it."
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
