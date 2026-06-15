package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunTryUnknownRepoErrors(t *testing.T) {
	if err := runTry([]string{"django"}); err == nil {
		t.Fatal("expected error for unknown repo")
	}
}

func TestRunTryWritesIntoCwd(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := runTry([]string{"react"}); err != nil {
		t.Fatalf("runTry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Fatalf(".mcp.json not written: %v", err)
	}
}
