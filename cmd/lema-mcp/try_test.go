package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func readServers(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s, _ := root["mcpServers"].(map[string]any)
	return s
}

func TestEnsureMCPTryJSONWritesPublicServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if _, err := ensureMCPTryJSON(path, "react-rfcs", "https://api.example"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// ADR-0097: the public server keys on `lema`, not the retired `lema-try`.
	srv, _ := readServers(t, path)["lema"].(map[string]any)
	if srv == nil {
		t.Fatalf("no lema server written")
	}
	if srv["command"] != "npx" {
		t.Errorf("command = %v, want npx", srv["command"])
	}
	env, _ := srv["env"].(map[string]any)
	if env["LEMA_MCP_MODE"] != "public" || env["LEMA_PUBLIC_REPO"] != "react-rfcs" || env["LEMA_PUBLIC_API_URL"] != "https://api.example" {
		t.Errorf("env wrong: %+v", env)
	}
}

// Re-running `try` refreshes the pinned repo on the SAME `lema` key (ADR-0097),
// and never writes the retired `lema-try` key.
func TestEnsureMCPTryJSONRefreshesPublicRepo(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if _, err := ensureMCPTryJSON(path, "react-rfcs", ""); err != nil {
		t.Fatalf("write1: %v", err)
	}
	if _, err := ensureMCPTryJSON(path, "rust-rfcs", ""); err != nil {
		t.Fatalf("write2: %v", err)
	}
	servers := readServers(t, path)
	if servers["lema-try"] != nil {
		t.Error("must not write the retired `lema-try` key")
	}
	env, _ := servers["lema"].(map[string]any)["env"].(map[string]any)
	if env["LEMA_PUBLIC_REPO"] != "rust-rfcs" {
		t.Errorf("re-run must update the pinned repo, got %v", env["LEMA_PUBLIC_REPO"])
	}
	if _, ok := env["LEMA_PUBLIC_API_URL"]; ok {
		t.Error("empty api url must be omitted from env")
	}
}

// An authed `lema` server (from `init`) is a superset that already serves the
// public tools, so `try` must NOT downgrade it: it returns errAuthedLemaPresent
// and leaves the authed entry untouched (ADR-0097 upgrade-in-place).
func TestEnsureMCPTryJSONDoesNotDowngradeAuthedLema(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	_ = os.WriteFile(path, []byte(`{"mcpServers":{"lema":{"command":"npx","args":["-y","lema-mcp@latest"]}}}`), 0o644)
	wrote, err := ensureMCPTryJSON(path, "react-rfcs", "")
	if !errors.Is(err, errAuthedLemaPresent) {
		t.Fatalf("err = %v, want errAuthedLemaPresent", err)
	}
	if wrote {
		t.Error("must not report a write when refusing to downgrade")
	}
	srv, _ := readServers(t, path)["lema"].(map[string]any)
	if srv == nil {
		t.Fatal("authed lema entry must be preserved")
	}
	if _, hasEnv := srv["env"]; hasEnv {
		t.Error("authed lema entry must be left untouched (no public env block added)")
	}
}
