package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestNudgeReminder(t *testing.T) {
	cases := []struct {
		name string
		in   guardInput
		fire bool
	}{
		{"edit go.mod", guardInput{ToolName: "Edit", ToolInput: map[string]any{"file_path": "go.mod"}}, true},
		{"write nested package.json", guardInput{ToolName: "Write", ToolInput: map[string]any{"file_path": "apps/web/package.json"}}, true},
		{"multiedit Cargo.toml", guardInput{ToolName: "MultiEdit", ToolInput: map[string]any{"file_path": "src-tauri/Cargo.toml"}}, true},
		{"edit ordinary code", guardInput{ToolName: "Edit", ToolInput: map[string]any{"file_path": "main.go"}}, false},
		{"edit a lock file", guardInput{ToolName: "Edit", ToolInput: map[string]any{"file_path": "package-lock.json"}}, false},
		{"bash install", guardInput{ToolName: "Bash", ToolInput: map[string]any{"command": "npm install left-pad"}}, false},
		{"no file_path", guardInput{ToolName: "Edit", ToolInput: map[string]any{}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg := nudgeReminder(c.in)
			if c.fire && msg == "" {
				t.Fatalf("expected a nudge, got none")
			}
			if !c.fire && msg != "" {
				t.Fatalf("expected silence, got %q", msg)
			}
			if c.fire && !strings.Contains(msg, "record_decision") {
				t.Fatalf("nudge should point at record_decision: %q", msg)
			}
		})
	}
}

func TestRunNudgeStdin(t *testing.T) {
	// A manifest edit → a non-blocking additionalContext nudge at record_decision.
	out := runNudgeWith(t, `{"tool_name":"Edit","tool_input":{"file_path":"go.mod"}}`)
	if !strings.Contains(out, "additionalContext") || !strings.Contains(out, "record_decision") {
		t.Fatalf("expected a record_decision nudge, got: %q", out)
	}
	if strings.Contains(out, "permissionDecision") {
		t.Fatalf("the nudge must not carry a permissionDecision: %q", out)
	}
	// An ordinary edit → silence.
	if got := runNudgeWith(t, `{"tool_name":"Edit","tool_input":{"file_path":"main.go"}}`); strings.TrimSpace(got) != "" {
		t.Fatalf("ordinary edit should not nudge, got: %q", got)
	}
	// Malformed → fail-open, silence.
	if got := runNudgeWith(t, "not json"); strings.TrimSpace(got) != "" {
		t.Fatalf("malformed input should emit nothing, got: %q", got)
	}
}

func runNudgeWith(t *testing.T, stdin string) string {
	t.Helper()
	rIn, wIn, _ := os.Pipe()
	if _, err := wIn.WriteString(stdin); err != nil {
		t.Fatal(err)
	}
	wIn.Close()
	rOut, wOut, _ := os.Pipe()
	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = rIn, wOut
	defer func() { os.Stdin, os.Stdout = origIn, origOut }()
	runNudge(nil)
	wOut.Close()
	b, _ := io.ReadAll(rOut)
	return string(b)
}
