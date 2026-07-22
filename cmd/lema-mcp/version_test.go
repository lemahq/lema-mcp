package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPackageVersionMatchesNPMAndMCPHandshake(t *testing.T) {
	packageJSON := filepath.Join("..", "..", "npm", "lema-mcp", "package.json")
	data, err := os.ReadFile(packageJSON)
	if err != nil {
		t.Fatal(err)
	}
	var npmPackage struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &npmPackage); err != nil {
		t.Fatal(err)
	}
	if Version != npmPackage.Version {
		t.Fatalf("Go package version = %q, npm package version = %q", Version, npmPackage.Version)
	}

	ctx := context.Background()
	// Both the authed stdio path and public-only path use this constructor, so
	// one initialize handshake pins the advertised version for both modes.
	server := newLemaMCPServer("version test")
	clientT, serverT := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "version-test", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	initialize := clientSession.InitializeResult()
	if initialize == nil || initialize.ServerInfo == nil {
		t.Fatal("MCP initialize result omitted serverInfo")
	}
	if initialize.ServerInfo.Version != Version {
		t.Fatalf("MCP serverInfo version = %q, Go package version = %q", initialize.ServerInfo.Version, Version)
	}
}

func TestNPMBuildCommandInjectsPackageVersionIntoGoBinary(t *testing.T) {
	scriptPath := filepath.Join("..", "..", "scripts", "build-npm.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `-X main.Version=$VERSION`) {
		t.Fatalf("%s does not inject the npm package version into main.Version", scriptPath)
	}
}
