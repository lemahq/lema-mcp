package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInitWritesAllThreeFiles(t *testing.T) {
	dir := t.TempDir()
	if err := runInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{".mcp.json", "AGENTS.md", filepath.Join(".claude", "settings.json")} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("expected %s to exist: %v", f, err)
		}
	}

	agents, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(agents), "record_decision") {
		t.Error("AGENTS.md is missing the capture protocol")
	}

	var mcp map[string]any
	mj, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	json.Unmarshal(mj, &mcp)
	servers, _ := mcp["mcpServers"].(map[string]any)
	if _, ok := servers["lema"]; !ok {
		t.Error(".mcp.json did not register the lema server")
	}
}

func TestRunInitIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := runInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	snap := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return string(b)
	}
	before := snap(".mcp.json") + snap("AGENTS.md") + snap(filepath.Join(".claude", "settings.json"))
	if err := runInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	after := snap(".mcp.json") + snap("AGENTS.md") + snap(filepath.Join(".claude", "settings.json"))
	if before != after {
		t.Error("second init changed files; expected idempotent no-op")
	}
}

func TestRunInitPreservesExistingConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"),
		[]byte(`{"mcpServers":{"other":{"command":"foo"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{dir}); err != nil {
		t.Fatal(err)
	}
	var mcp map[string]any
	mj, _ := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	json.Unmarshal(mj, &mcp)
	servers, _ := mcp["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Error("init clobbered the pre-existing 'other' server")
	}
	if _, ok := servers["lema"]; !ok {
		t.Error("init did not add the lema server alongside the existing one")
	}
}

func TestRunInitRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runInit([]string{dir}); err == nil {
		t.Error("expected init to refuse a malformed .mcp.json rather than discard it")
	}
}
