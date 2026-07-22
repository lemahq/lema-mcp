package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lemahq/lema-mcp/internal/source"
)

// publicServerKey is the .mcp.json key for the public (tokenless) server. Per
// ADR-0097 it is `lema` (not the old subordinating `lema-try`): to a funnel user
// with nothing else connected, this IS lema. The authed `init` server uses the
// same key and is a SUPERSET (it registers the public tools too), so the two never
// need to coexist — `try` upgrades in place and never downgrades an authed entry
// (see ensureMCPTryJSON / isPublicTryEntry).
const publicServerKey = "lema"

// publicServerInstructions is the server-level steering the host injects into the
// agent's context — the channel a docs MCP uses to win the default reach, which the
// public funnel left empty (ADR-0097). It states the failure mode (a project's
// recorded rationale and rejected alternatives are not recoverable from the code;
// answering "why" from model recall reconstructs them, sometimes wrongly) as fact,
// not as an instruction to Claude — so it stays clear of the Directory banned-phrase
// rule. It names no specific tool, so it survives the why_decided drop.
const publicServerInstructions = "lema answers why upstream open-source projects — React, Kubernetes (k8s), Rust, Vue, and Go — made their design decisions, and whether a direction was already ruled out, grounded in each project's own recorded RFC/KEP deliberation with GitHub citations. A coding agent cannot recover a project's rationale or its rejected alternatives from the source code; producing a \"why\" from model recall reconstructs it — fluently, sometimes wrongly. This server returns the project's actual record instead. It holds reasoning (why a decision was made, what was rejected), not API syntax or code samples — a documentation tool is the right place for those. When the record is silent it says so; that means \"unknown,\" not \"approved.\" Covered today: React, Kubernetes, Rust, Vue, Go."

// errAuthedLemaPresent signals that an authed `lema` server (from `init`) already
// exists in .mcp.json. `try` must not downgrade it — the authed server already
// serves the public tools, so the user already has this capability.
var errAuthedLemaPresent = errors.New("authed lema server already present")

// isPublicTryEntry reports whether an existing .mcp.json server entry is the
// public-demo one (vs. an authed `init` entry). The public entry carries
// env.LEMA_MCP_MODE=public; the authed entry has no env block.
func isPublicTryEntry(entry map[string]any) bool {
	env, _ := entry["env"].(map[string]any)
	return env != nil && env["LEMA_MCP_MODE"] == "public"
}

// ensureMCPTryJSON registers the read-only public-demo server under the `lema` key
// (ADR-0097), with the public-mode env block. Re-runnable: it refreshes the pinned
// repo. apiURL is written only when non-empty (the published binary's baked default
// covers the empty case). If an AUTHED `lema` entry already exists it is left
// untouched and errAuthedLemaPresent is returned (upgrade-in-place: never downgrade
// the superset authed server to the public-only one).
func ensureMCPTryJSON(path, slug, apiURL string) (bool, error) {
	root, err := readJSONObject(path)
	if err != nil {
		return false, err
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if existing, ok := servers[publicServerKey].(map[string]any); ok && !isPublicTryEntry(existing) {
		return false, errAuthedLemaPresent
	}
	env := map[string]any{"LEMA_MCP_MODE": "public", "LEMA_PUBLIC_REPO": slug}
	if apiURL != "" {
		env["LEMA_PUBLIC_API_URL"] = apiURL
	}
	servers[publicServerKey] = map[string]any{
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
		fmt.Fprintln(os.Stderr, "lema-mcp: public mode but no LEMA_PUBLIC_API_URL / baked default — the public tools will error until set")
	}
	server := newLemaMCPServer(publicServerInstructions)
	mcp.AddTool(server, checkApproachTool, checkApproach)
	fmt.Fprintln(os.Stderr, "lema-mcp: public demo mode — why React/Kubernetes/Rust/Vue/Go decided things + what they ruled out, no account")
	return server.Run(context.Background(), &mcp.StdioTransport{})
}

// runTry writes the read-only public-demo server into ./.mcp.json + prints next steps.
func runTry(args []string) error {
	if len(args) < 1 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("usage: lema-mcp try <react|kubernetes|rust|vue|go>")
	}
	repo := strings.ToLower(strings.TrimSpace(args[0]))
	slug, ok := publicRepoSlugs[repo]
	if !ok {
		return fmt.Errorf("unknown repo %q; supported: react, kubernetes, rust, vue, go", args[0])
	}
	if _, err := ensureMCPTryJSON(".mcp.json", slug, resolvePublicAPIURL()); err != nil {
		if errors.Is(err, errAuthedLemaPresent) {
			fmt.Println("You already have the full lema server in .mcp.json — it includes the public React/Kubernetes/Rust/Vue/Go tools, so there's nothing to add.")
			return nil
		}
		return err
	}
	fmt.Printf("+ .mcp.json (lema — read-only public demo: %s)\n", slug)
	if resolvePublicAPIURL() == "" {
		fmt.Println("  note: set LEMA_PUBLIC_API_URL to the public lema-api (the published binary will carry a default).")
	}
	fmt.Println("Next: reload MCP servers in your agent — in Claude Code, run /mcp — then ask, e.g.:")
	fmt.Printf("  \"why did %s decide X?\"\n", repo)
	fmt.Println("(First run downloads the binary — a few seconds.)")
	fmt.Println("Liked it? `npx lema-mcp init` adds decision capture + the never-reopen guard for YOUR repo.")
	return nil
}
