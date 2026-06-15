package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lemahq/lema-mcp/internal/source"
)

// ensureMCPTryJSON registers a read-only public-demo server under the distinct
// `lema-try` key (NOT `lema`, so `try` and `init` coexist), with the public-mode
// env block. Re-runnable: it refreshes the pinned repo. apiURL is written only
// when non-empty (the published binary's baked default covers the empty case).
func ensureMCPTryJSON(path, slug, apiURL string) (bool, error) {
	root, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	env := map[string]any{"LEMA_MCP_MODE": "public", "LEMA_PUBLIC_REPO": slug}
	if apiURL != "" {
		env["LEMA_PUBLIC_API_URL"] = apiURL
	}
	servers["lema-try"] = map[string]any{
		"command": "npx",
		"args":    []any{"-y", "lema-mcp@" + versionOrLatest()},
		"env":     env,
	}
	root["mcpServers"] = servers
	return true, writeJSON(path, root)
}

// resolvePublicAPIURL is the single source of truth for the public api root:
// the env override, else the compiled-in default (empty until prod-public is baked).
func resolvePublicAPIURL() string {
	if u := os.Getenv("LEMA_PUBLIC_API_URL"); u != "" {
		return u
	}
	return defaultPublicAPIURL
}

// runPublicOnlyServer is the LEMA_MCP_MODE=public boot path (the `try` install):
// it skips ALL local setup and registers ONLY the tokenless public tools.
func runPublicOnlyServer() error {
	if u := resolvePublicAPIURL(); u != "" {
		publicSrc = source.NewPublic(u, nil)
	} else {
		fmt.Fprintln(os.Stderr, "lema-mcp: public mode but no LEMA_PUBLIC_API_URL / baked default — public_ask will error until set")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "lema-mcp", Version: "0.7.0"}, nil)
	mcp.AddTool(server, publicAskTool, publicAsk)
	mcp.AddTool(server, whyNotPublicTool, whyNotPublic)
	fmt.Fprintln(os.Stderr, "lema-mcp: public demo mode — public_ask + why_not_public over React/k8s/Rust, no account")
	return server.Run(context.Background(), &mcp.StdioTransport{})
}

// runTry writes the read-only public-demo server into ./.mcp.json + prints next steps.
func runTry(args []string) error {
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("usage: lema-mcp try <react|kubernetes|rust>")
	}
	repo := strings.ToLower(strings.TrimSpace(args[0]))
	slug, ok := publicRepoSlugs[repo]
	if !ok {
		return fmt.Errorf("unknown repo %q; supported: react, kubernetes, rust", args[0])
	}
	if _, err := ensureMCPTryJSON(".mcp.json", slug, resolvePublicAPIURL()); err != nil {
		return err
	}
	fmt.Printf("+ .mcp.json (lema-try — read-only public demo: %s)\n", slug)
	if resolvePublicAPIURL() == "" {
		fmt.Println("  note: set LEMA_PUBLIC_API_URL to the public lema-api (the published binary will carry a default).")
	}
	fmt.Println("Next: reload MCP servers in your agent — in Claude Code, run /mcp — then ask, e.g.:")
	fmt.Printf("  \"why did %s settle on X?\"  — or call why_not_public before you propose a pattern.\n", repo)
	fmt.Println("(First run downloads the binary — a few seconds.)")
	fmt.Println("Liked it? `npx lema-mcp init` adds decision capture + the never-reopen guard for YOUR repo.")
	return nil
}
