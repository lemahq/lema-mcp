package main

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/lemahq/lema-mcp/internal/source"
)

// guardCheckOutput is the /api/guard response: whether a Claude Code tool-call
// reaches for a settled (CLOSED) decision, and the cited closed atom(s) the
// terminal renders. Advisory only — a verdict here is never an auto-block; a
// human resolves it (the terminal hosts the human, unlike the unattended CLI hook).
type guardCheckOutput struct {
	Decided bool          `json:"decided"`
	ID      string        `json:"id,omitempty"` // pending id to poll /api/guard/result on a hit
	Closed  []source.Atom `json:"closed,omitempty"`
}

// httpGuard evaluates a Claude Code tool-call (the PreToolUse payload: tool_name +
// tool_input) against the team's CLOSED decisions and returns the interception for
// the terminal to render. It reuses the SAME evaluation the PreToolUse hook runs
// (guardQuery -> evaluateGuard) so the terminal renders exactly what the engine
// already decides — one matcher, no second verdict door (ADR-0052/0094). Advisory:
// it reports a reached-for closed decision; it never returns allow/deny. POST only.
func httpGuard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in guardInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Honor the LEMA_GUARD_MODE kill switch the same way runGuard does: off → silent.
	mode := os.Getenv(guardModeEnvVar)
	if mode == "" {
		mode = guardModeContext
	}
	var closed []source.Atom
	if capture != nil {
		closed = capture.ClosedAtoms()
	}
	// The same evaluation the PreToolUse hook runs — the matched CLOSED atom above
	// the context floor. The terminal renders that atom; a human resolves it.
	_, atom := evaluateGuard(closed, guardQuery(in.ToolInput), mode)
	out := guardCheckOutput{Decided: atom != nil}
	if atom != nil {
		out.Closed = []source.Atom{*atom}
		// Open a pending interception for the terminal to render and a human to
		// resolve; the hook polls /api/guard/result?id= for that resolution.
		out.ID = guardPendings.add(in.ToolName, out.Closed)
	}
	writeJSONResp(w, out)
}
