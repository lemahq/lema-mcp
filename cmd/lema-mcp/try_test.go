package main

import (
	"encoding/json"
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
	srv, _ := readServers(t, path)["lema-try"].(map[string]any)
	if srv == nil {
		t.Fatalf("no lema-try server written")
	}
	if srv["command"] != "npx" {
		t.Errorf("command = %v, want npx", srv["command"])
	}
	env, _ := srv["env"].(map[string]any)
	if env["LEMA_MCP_MODE"] != "public" || env["LEMA_PUBLIC_REPO"] != "react-rfcs" || env["LEMA_PUBLIC_API_URL"] != "https://api.example" {
		t.Errorf("env wrong: %+v", env)
	}
}

func TestEnsureMCPTryJSONCoexistsAndUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	_ = os.WriteFile(path, []byte(`{"mcpServers":{"lema":{"command":"npx","args":["-y","lema-mcp@latest"]}}}`), 0o644)
	if _, err := ensureMCPTryJSON(path, "react-rfcs", ""); err != nil {
		t.Fatalf("write1: %v", err)
	}
	if _, err := ensureMCPTryJSON(path, "rust-rfcs", ""); err != nil {
		t.Fatalf("write2: %v", err)
	}
	servers := readServers(t, path)
	if servers["lema"] == nil {
		t.Error("existing `lema` server must be preserved")
	}
	env, _ := servers["lema-try"].(map[string]any)["env"].(map[string]any)
	if env["LEMA_PUBLIC_REPO"] != "rust-rfcs" {
		t.Errorf("re-run must update the pinned repo, got %v", env["LEMA_PUBLIC_REPO"])
	}
	if _, ok := env["LEMA_PUBLIC_API_URL"]; ok {
		t.Error("empty api url must be omitted from env")
	}
}
