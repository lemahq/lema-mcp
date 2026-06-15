package main

import (
	"os"
	"path/filepath"
	"testing"
)

// These tests pin the ADR-0060 resolved-question-1 channel: the per-user
// ~/.config/lema/credentials file feeds hosted mode when shell env doesn't —
// the GUI-launched-editor case — and NEVER overrides explicit env.

func writeCredsFile(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "lema")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "credentials")
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestResolveHostedConfigFromFile(t *testing.T) {
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")
	writeCredsFile(t, "# lema hosted credentials\nLEMA_API_URL=https://api.example.test\nLEMA_API_TOKEN=lema_live_abc\n", 0o600)

	url, token, usedFile := resolveHostedConfig()
	if url != "https://api.example.test" || token != "lema_live_abc" {
		t.Errorf("resolved (%q, %q), want file values", url, token)
	}
	if !usedFile {
		t.Error("usedFile = false, want true")
	}
}

func TestResolveHostedConfigEnvWins(t *testing.T) {
	writeCredsFile(t, "LEMA_API_URL=https://file.example.test\nLEMA_API_TOKEN=lema_live_file\n", 0o600)
	t.Setenv("LEMA_API_URL", "https://env.example.test")
	t.Setenv("LEMA_API_TOKEN", "lema_live_env")

	url, token, usedFile := resolveHostedConfig()
	if url != "https://env.example.test" || token != "lema_live_env" {
		t.Errorf("resolved (%q, %q), want env values (env always wins)", url, token)
	}
	if usedFile {
		t.Error("usedFile = true with full env config")
	}
}

func TestResolveHostedConfigFileFillsGaps(t *testing.T) {
	// URL from env (e.g. a stage override), token from the file.
	writeCredsFile(t, "LEMA_API_TOKEN=lema_live_filetoken\n", 0o600)
	t.Setenv("LEMA_API_URL", "https://stage.example.test")
	t.Setenv("LEMA_API_TOKEN", "")

	url, token, usedFile := resolveHostedConfig()
	if url != "https://stage.example.test" || token != "lema_live_filetoken" || !usedFile {
		t.Errorf("resolved (%q, %q, usedFile=%v), want env URL + file token", url, token, usedFile)
	}
}

func TestResolveHostedConfigNoFileNoEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no credentials file
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")

	url, token, usedFile := resolveHostedConfig()
	if url != "" || token != "" || usedFile {
		t.Errorf("resolved (%q, %q, %v), want all-empty (local mode)", url, token, usedFile)
	}
}

func TestReadCredentialsFileParsing(t *testing.T) {
	path := writeCredsFile(t, "\n# comment\nLEMA_API_TOKEN = lema_live_x \nmalformed line\nEXTRA=1\n", 0o600)
	creds, err := readCredentialsFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if creds["LEMA_API_TOKEN"] != "lema_live_x" {
		t.Errorf("token = %q, want trimmed value", creds["LEMA_API_TOKEN"])
	}
	if _, ok := creds["malformed line"]; ok {
		t.Error("malformed line should be skipped")
	}
}
