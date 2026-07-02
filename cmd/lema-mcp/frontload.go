package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
)

// frontload.go is the `lema-mcp frontload` subcommand — beat 1 of the session
// lifecycle (agent-session-loop P1). It runs as a UserPromptSubmit hook: at the
// moment the user submits a prompt, it retrieves ONLY the recorded decisions
// relevant to that prompt and injects them as context BEFORE the agent acts, so
// the agent starts with the team's already-settled reasoning instead of
// reconstructing (and sometimes inventing) a "why" from model recall.
//
// Two structural facts this implements:
//   - The prompt text IS the relevance signal (UserPromptSubmit carries it;
//     SessionStart does not). So UserPromptSubmit is the shippable primary; the
//     SessionStart git-derived warm-start stays deferred behind a precision probe
//     (spec-01 §3 Task F) — coarser scope risks a soft dump at boot.
//   - The "not a dump" / abstain invariant is owned by the SERVER. /retrieve
//     deliberately returns raw ranked atoms with no relevance floor
//     (retrieve.go), so it cannot abstain; /ask applies the calibrated relevance
//     floor (ask.go DefaultRelevanceFloor) and returns NO sources when nothing
//     clears it. So frontload goes through Ask and treats an empty Sources set as
//     the honest abstain: it injects zero bytes. The floor lives in one place
//     (the server), not re-derived here.
//
// The whole feature is dark behind LEMA_FUSE_FRONTLOAD (default OFF) and is
// fail-open everywhere — a frontload hook must never wedge a turn or pollute the
// agent's context, so any error/timeout/silence is a no-op that injects nothing.
//
// Wire it as a UserPromptSubmit hook in .claude/settings.json once the flag is on
// (stdout of a UserPromptSubmit hook is injected as agent context):
//
//	"UserPromptSubmit": [{ "hooks": [{ "type": "command",
//	  "command": "lema-mcp frontload" }]}]

const (
	frontloadFlagEnv     = "LEMA_FUSE_FRONTLOAD" // master switch, default OFF (dark in prod)
	frontloadMaxSources  = 5                     // hard cap on injected atoms — never the whole graph
	frontloadMaxQueryLen = 2000                  // cap the scope-query length (runes)
)

// frontloadTimeout bounds the whole frontload (the HTTP client and the request
// context) so a UserPromptSubmit hook can never hang the user's turn on a slow
// /ask synthesis. On timeout the hook fails open and injects nothing.
const frontloadTimeout = 10 * time.Second

// frontloadEnabled reports whether the frontload reader is on. Default OFF, read
// per invocation — the flag-flip-not-a-build lever, mirroring pushEnabled.
func frontloadEnabled() bool {
	v := os.Getenv(frontloadFlagEnv)
	return v == "1" || v == "true"
}

// frontloadInput is the subset of the UserPromptSubmit hook stdin payload the
// reader needs. The prompt text is the scope signal; other fields are ignored.
type frontloadInput struct {
	Prompt string `json:"prompt"`
}

// buildScopeQuery turns a hook payload into a retrieval query. For UserPromptSubmit
// the prompt text IS the query (true relevance) — trimmed and length-capped. A
// blank prompt yields "" so the caller no-ops. Deterministic transform (code, not
// the model — the global rule: deterministic transforms are code).
func buildScopeQuery(in frontloadInput) string {
	q := strings.TrimSpace(in.Prompt)
	if r := []rune(q); len(r) > frontloadMaxQueryLen {
		q = strings.TrimSpace(string(r[:frontloadMaxQueryLen]))
	}
	return q
}

// frontloadRunner is runFrontload's testable core with the I/O seam injected (the
// hosted ask call + whether credentials resolved). The shell runFrontload wires
// the real implementation; tests pass fakes.
type frontloadRunner struct {
	ask      func(ctx context.Context, query string) (source.AskResult, error)
	canQuery bool // credentials + workspace resolved
}

// run executes the frontload for one hook event and returns the context block to
// inject on stdout ("" = inject nothing). Fail-open throughout: no credentials,
// no scope signal, a retrieval error, or a server abstain (no sources) all return
// "" — silence is the honest answer when the record can't ground the prompt
// (the public-context-mcp honesty boundary: abstain is a feature, never noise).
func (r frontloadRunner) run(ctx context.Context, in frontloadInput) string {
	if !r.canQuery {
		return "" // nowhere to query — fail-open
	}
	query := buildScopeQuery(in)
	if query == "" {
		return "" // no scope signal in this event
	}
	res, err := r.ask(ctx, query)
	if err != nil {
		return "" // a retrieval failure must not wedge the turn
	}
	sources := capSources(res.Sources, frontloadMaxSources)
	if len(sources) == 0 {
		return "" // the engine abstained server-side — inject nothing
	}
	return renderFrontload(res.Answer, sources)
}

// capSources keeps at most max sources, preserving the server's relevance order.
func capSources(sources []source.AskSource, max int) []source.AskSource {
	if len(sources) > max {
		return sources[:max]
	}
	return sources
}

// renderFrontload builds the stdout context block: the cited synthesized why,
// followed by the cited sources (≤ frontloadMaxSources, in relevance order). It
// returns "" when there is nothing to say so the caller injects zero bytes.
func renderFrontload(answer string, sources []source.AskSource) string {
	answer = strings.TrimSpace(answer)
	if answer == "" && len(sources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Relevant recorded decisions from lema (your team's recorded reasoning — what was decided and why; cited, not reconstructed):\n\n")
	if answer != "" {
		b.WriteString(answer)
		b.WriteString("\n")
	}
	if len(sources) > 0 {
		b.WriteString("\nSources:\n")
		for _, s := range sources {
			line := fmt.Sprintf("[%d] %s", s.N, strings.TrimSpace(s.Ref))
			if t := strings.TrimSpace(s.Type); t != "" {
				line += " (" + t + ")"
			}
			if loc := strings.TrimSpace(firstNonEmpty(s.URL, s.Locator)); loc != "" {
				line += " — " + loc
			}
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// runFrontload is the `lema-mcp frontload` UserPromptSubmit-hook body — the thin
// I/O shell over the tested frontloadRunner. Dark unless LEMA_FUSE_FRONTLOAD. It
// reads the prompt from stdin, resolves the hosted credentials + target workspace
// (LEMA_WORKSPACE_ID), retrieves the relevant recorded decisions via the same
// hosted /ask the agent uses, and writes the cited context block to stdout (the
// UserPromptSubmit injection channel). Always exits 0 and writes nothing on
// abstain/error — a reader hook must never wedge or pollute a session.
func runFrontload(args []string) {
	if !frontloadEnabled() {
		return
	}
	data, ok := readStopStdin(3 * time.Second) // reuse push.go's bounded stdin reader
	if !ok {
		return
	}
	var in frontloadInput
	if json.Unmarshal(data, &in) != nil {
		return
	}
	apiURL, token, _ := resolveHostedConfig()
	workspaceID := resolveWorkspaceID()
	client := &http.Client{Timeout: frontloadTimeout}
	hosted := source.NewHosted(apiURL, token, client)
	r := frontloadRunner{
		ask: func(ctx context.Context, query string) (source.AskResult, error) {
			return hosted.Ask(ctx, query, scopeWorkspaceIDs(workspaceID))
		},
		canQuery: apiURL != "" && token != "" && workspaceID != "",
	}
	ctx, cancel := context.WithTimeout(context.Background(), frontloadTimeout)
	defer cancel()
	if out := r.run(ctx, in); out != "" {
		fmt.Fprint(os.Stdout, out)
	}
}

// scopeWorkspaceIDs focuses retrieval on the configured workspace, or nil to let
// the server resolve the caller's full scope.
func scopeWorkspaceIDs(ws string) []string {
	if ws == "" {
		return nil
	}
	return []string{ws}
}
