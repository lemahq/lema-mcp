package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
)

// guardEndpointEnvVar, when set, switches the PreToolUse hook into terminal mode:
// instead of evaluating locally and only ever nudging, the hook POSTs the tool-call
// to the lema terminal's `serve --http` sidecar and blocks for an attended human to
// resolve the interception the terminal renders. See guardViaTerminal.
const guardEndpointEnvVar = "LEMA_GUARD_ENDPOINT"

// Terminal-mode poll tunables — package vars so tests can shrink the interval.
var (
	guardPollInterval = 750 * time.Millisecond
	guardPollTimeout  = 10 * time.Minute
)

// guardHTTPClient bounds a single POST/GET so one hung request can't wedge the agent;
// the poll loop is bounded separately by guardPollTimeout.
var guardHTTPClient = &http.Client{Timeout: 5 * time.Second}

// guardViaTerminal handles a Claude Code tool-call when the hook runs inside the lema
// terminal (LEMA_GUARD_ENDPOINT set). It POSTs the tool-call to the terminal's
// serve --http sidecar; if the engine flags a settled decision the call reaches for,
// it blocks polling /api/guard/result until the human resolves the interception the
// terminal renders, then maps that resolution to the PreToolUse decision
// (mapResolution): :respect binds a deny, :override proceeds.
//
// The deny is legitimate here precisely because a human bound it — the locked
// invariant is that a tool-call is discarded only after an interactive-human
// resolution, never by the matcher alone, so the unattended CLI hook still never
// denies (ADR-0052). Fail-open throughout: any transport error, a non-decision, or a
// timeout proceeds silently — an advisory layer must never wedge the agent on its own
// infrastructure.
func guardViaTerminal(client *http.Client, endpoint string, in guardInput) *guardOutput {
	base := strings.TrimRight(endpoint, "/")
	body, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	resp, err := client.Post(base+"/api/guard", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil // fail open — sidecar unreachable
	}
	var decided guardCheckOutput
	derr := json.NewDecoder(resp.Body).Decode(&decided)
	resp.Body.Close()
	if derr != nil || !decided.Decided || decided.ID == "" {
		return nil // nothing settled was reached — proceed silently
	}
	// A settled decision was reached. Block until the human resolves it (or time out
	// and fail open). The terminal discovers the pending via /api/guard/pending and
	// POSTs /api/guard/resolve when the human presses :respect / :override.
	deadline := time.Now().Add(guardPollTimeout)
	for time.Now().Before(deadline) {
		if res, ok := pollGuardResult(client, base, decided.ID); ok && res.Resolved {
			return mapResolution(res.Resolution, decided.Closed)
		}
		time.Sleep(guardPollInterval)
	}
	return nil // the human never resolved — fail open
}

// guardResultResp is the /api/guard/result poll body.
type guardResultResp struct {
	Resolved   bool   `json:"resolved"`
	Resolution string `json:"resolution"`
	Why        string `json:"why"`
}

// pollGuardResult fetches the resolution state for one interception; ok=false on any
// transport/decode error or non-200, which the caller treats as "not yet resolved"
// and keeps polling until the deadline.
func pollGuardResult(client *http.Client, base, id string) (guardResultResp, bool) {
	resp, err := client.Get(base + "/api/guard/result?id=" + url.QueryEscape(id))
	if err != nil {
		return guardResultResp{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return guardResultResp{}, false
	}
	var out guardResultResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return guardResultResp{}, false
	}
	return out, true
}

// mapResolution turns the human's terminal resolution into the PreToolUse decision.
//
//	:respect  -> deny, human-bound. The human read the settled decision the call
//	             reaches for and chose to keep it, so this tool-call is discarded —
//	             the invariant's interactive-human resolution. The matcher never
//	             reaches this on its own. The deny CITES the decision.
//	:override -> proceed silently (nil). The human is intentionally superseding; the
//	             terminal records the supersession (record_decision with the human's
//	             why) so the next agent isn't re-flagged. That write is the terminal's
//	             job, not the fail-open hook's.
//
// Anything else (empty / unknown) fails open.
func mapResolution(resolution string, closed []source.Atom) *guardOutput {
	if resolution != "respect" {
		return nil
	}
	note := "you chose to keep a decision your team already settled."
	if len(closed) > 0 {
		if closed[0].ClosedNote != "" {
			note = closed[0].ClosedNote
		} else if closed[0].Text != "" {
			note = "already ruled out: " + closed[0].Text
		}
	}
	return &guardOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName:            "PreToolUse",
		PermissionDecision:       "deny",
		PermissionDecisionReason: "lema — " + note,
	}}
}
