// boundary_bind_notice.go is MC-7's boundary-ambient nudge: at a session
// boundary, one stderr line points at the pre-checked bind-pending batch —
// rulings this run's work produced that a human can now confirm with one
// click. Fail-open everywhere (HC-7): any resolution or transport failure is
// collectorDebugf-and-silent, and this must never block a boundary or wait
// longer than bindNoticeTimeout. It only PRINTS a link — it cannot bind
// (ADR-0141: the confirm click lives in-app or nowhere).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// bindNoticeTimeout bounds each of the notice's two network steps (workspace
// resolution, then the bind-pending lookup) — tight, because this runs inside
// hook budgets immediately after the boundary checkpoint sync.
const bindNoticeTimeout = 3 * time.Second

// notifyBindPending is collector.go's boundary hook, called right after
// syncOnBoundary. It resolves the same workspace the checkpoint just synced
// to — via the immutable-receipt target resolver syncCheckpoint itself uses,
// so the notice can never point at a different workspace than the sync did —
// and, only if that resolves, checks for a bind-pending batch.
func notifyBindPending(dir string, ev collectorEnvelope) {
	switch ev.Kind {
	case "pre_compact", "stop", "session_end":
	default:
		return
	}
	cwd := ev.Evidence["cwd"]
	if cwd == "" {
		return
	}
	s := newCollectorSyncerForCWD(cwd)
	if s == nil || s.runtime == nil {
		collectorDebugf("bind notice skipped: hosted target not configured")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), bindNoticeTimeout)
	defer cancel()
	// The notice must read the workspace captures are actually pushed to:
	// newHostedRecorder calls pushDecisions(..., receipt.RepositoryWorkspaceID,
	// records) in record_decision.go. If that push target ever changes, this
	// must change with it.
	workspaceID, err := withResolvedTarget(ctx, s.runtime.targets, s.runtime.targetInput,
		func(_ context.Context, receipt targetContext) (string, error) {
			return receipt.RepositoryWorkspaceID, nil
		})
	if err != nil {
		collectorDebugf("bind notice skipped: workspace not resolved: %v", err)
		return
	}
	notifyBindPendingTo(os.Stderr, s.apiURL, s.token, workspaceID, settleWebURL())
}

// notifyBindPendingTo does the actual GET and, only when count > 0, writes the
// notice to w. Every failure — timeout, transport error, non-200, malformed
// body — is silent: this is a best-effort nudge, never a blocking or noisy
// path.
func notifyBindPendingTo(w io.Writer, apiURL, token, workspaceID, appURL string) {
	ctx, cancel := context.WithTimeout(context.Background(), bindNoticeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		apiURL+"/workspaces/"+workspaceID+"/decisions/bind-pending", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		collectorDebugf("bind notice skipped: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var body struct {
		Count int `json:"count"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || body.Count == 0 {
		return
	}
	fmt.Fprintf(w, "lema: %d ruling(s) await one-click binding → %s/decisions/bind-pending\n",
		body.Count, appURL)
}
