package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// distill.go is the `lema-mcp distill` subcommand — Stage 3 of the session-end
// distiller (ADR-0140), the CLIENT half of the dogfood loop. It is a separate
// concern from `push` (Signal A stays its own subcommand): where push emits a
// deterministic structural draft, distill ships the scrubbed free-form
// DELIBERATION so the server's ingest.Extractor can harvest the `why` the agent
// forgot to record — the amnesia fix ADR-0140 rules for.
//
// It runs as a Stop hook. At session end it (1) asks the hosted API whether the
// env-wide WorkOS flag `lema-session-distill` is on — the gate, checked BEFORE
// anything is read; (2) scans + SCRUBS the session transcript locally, assembling
// the user/assistant prose turns into deliberation text; and (3) POSTs that to
// POST /workspaces/{id}/ingest-session as the authed programmatic principal. The
// server mints `proposed` atoms a human later accepts (ADR-0135; the accept binds,
// ADR-0125).
//
// Two hard rules, both load-bearing:
//   - PRIVACY / gate-before-read: the transcript is NEVER opened, scrubbed, or
//     transmitted unless the gate says the flag is on. The flag is the dogfood
//     consent model — the fuller shipped-to-others consent UX (per-repo allowlist,
//     `lema hooks install`) is deferred (ADR-0140, out of scope for Stage 3).
//   - FAIL-OPEN: every failure path is a silent no-op and the whole op is bounded
//     by pushTimeout, so a distill can never wedge or delay a session's turn-end.
//
// The local binary stays LLM-free and no-network for model work: it only scans,
// scrubs, and POSTs — all extraction is server-side (ADR-0140). It reuses push.go's
// stopHookInput / readStopStdin / pushTimeout and sessions.go's streaming scanner +
// scrubSecrets, so it adds no new transcript machinery.

// distilled is the scrubbed, assembled deliberation the client ships for one
// session: the prose text the extractor runs over, plus a best-effort repo label
// (the server accepts repo for forward compatibility, ADR-0140). Text is already
// secret-scrubbed and payload-bounded — nothing further leaves this boundary.
type distilled struct {
	Text string
	Repo string
}

// distillRunner is runDistill's testable core with the I/O seams injected (the
// gate, the scan+scrub, the HTTP post, and whether credentials resolved). The
// shell runDistill wires the real implementations; tests pass fakes. Mirrors
// pushRunner deliberately so the fail-open/gate-first discipline is identical.
type distillRunner struct {
	// gate reports whether the distiller is on for this deployment (the
	// lema-session-distill WorkOS flag, via the API pre-check). Checked BEFORE
	// scan, so a disabled distiller never reads or transmits the transcript. A nil
	// gate fails closed (treated as off) — the distiller must never run ungated.
	gate    func(ctx context.Context) bool
	scan    func(path string) (distilled, error)
	post    func(ctx context.Context, sessionID string, d distilled) (int, error)
	canPush bool // credentials + workspace resolved
}

// run executes the distiller for one Stop event and returns the number of proposed
// atoms the server harvested (0 for any no-op). Every failure path is a fail-open
// no-op — a Stop hook must never wedge a session — and it never blocks the stop:
// it harvests silently, leaving the human's in-app accept as the only judgment.
func (r distillRunner) run(ctx context.Context, in stopHookInput) int {
	if in.StopHookActive || strings.TrimSpace(in.TranscriptPath) == "" {
		return 0 // re-entrant stop, or nothing to read
	}
	if strings.TrimSpace(in.SessionID) == "" {
		return 0 // no session id — nothing to key the source by (ADR-0064 locator)
	}
	if !r.canPush {
		return 0 // no credentials/workspace resolved — nowhere to send; fail-open
	}
	if r.gate == nil || !r.gate(ctx) {
		return 0 // distiller off (lema-session-distill) or no gate wired — fail-closed, BEFORE any read/scrub/transmit
	}
	d, err := r.scan(in.TranscriptPath)
	if err != nil || strings.TrimSpace(d.Text) == "" {
		return 0 // unreadable transcript or no prose deliberation to harvest
	}
	n, err := r.post(ctx, in.SessionID, d)
	if err != nil {
		return 0 // a post failure (incl. the dark 404 when the flag is off server-side) must not wedge the session
	}
	return n
}

// runDistillWithRuntime is the `lema-mcp distill` Stop-hook body — the thin I/O
// shell over the tested distillRunner. Its process boundary has already loaded
// hosted credentials and built the target provider; this function resolves
// exactly one immutable repository target and — only when the hosted
// env-wide WorkOS flag lema-session-distill is on (the gate) — scans + scrubs the
// transcript and POSTs the deliberation. Always returns (exit 0) and never writes
// a block decision to stdout: a distiller failure must never wedge a session, and
// it harvests silently rather than nagging (the human's in-app accept is the only
// judgment).
//
// Wire it as a Stop hook in .claude/settings.json (it stays dark until the flag is
// flipped on, so it is safe to install ahead of turn-on):
//
//	"Stop": [{ "matcher": "", "hooks": [{ "type": "command",
//	  "command": "lema-mcp distill" }]}]
func runDistillWithRuntime(args []string, runtime hostedWriteRuntime) {
	data, ok := readStopStdin(3 * time.Second)
	if !ok {
		return
	}
	var in stopHookInput
	if json.Unmarshal(data, &in) != nil {
		return
	}
	if in.StopHookActive || strings.TrimSpace(in.TranscriptPath) == "" || strings.TrimSpace(in.SessionID) == "" {
		return
	}
	// Bound the whole op with pushTimeout so a slow/hung API can never delay the
	// agent's turn-end (the stdin read is already bounded; the gate pre-check and
	// the POST share this budget). The gate fires only inside run(), after the
	// cheap re-entrant/no-transcript/no-session/no-credentials checks — so a no-op
	// Stop costs no WorkOS call and never opens the transcript.
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	_, _ = withResolvedTarget(ctx, runtime.targets, runtime.targetInput, func(ctx context.Context, receipt targetContext) (struct{}, error) {
		r := distillRunner{
			gate: func(ctx context.Context) bool {
				return sessionDistillEnabled(ctx, runtime.client, runtime.apiURL, runtime.token)
			},
			scan: scanTranscriptForDistill,
			post: func(ctx context.Context, sessionID string, d distilled) (int, error) {
				return postSessionDistill(ctx, runtime.client, runtime.apiURL, runtime.token, receipt.RepositoryWorkspaceID, sessionID, d)
			},
			canPush: runtime.apiURL != "" && runtime.token != "",
		}
		if n := r.run(ctx, in); n > 0 {
			fmt.Fprintf(os.Stderr, "lema-mcp distill: harvested %d proposed atom(s) from this session — review and accept in-app to confirm and record them\n", n)
		}
		return struct{}{}, nil
	})
}

// distillWithTarget is the hosted ingest boundary. Resolution happens before
// any ingest operation HTTP and the deliberation is sent only to the receipt's
// repository leaf.
func distillWithTarget(ctx context.Context, runtime hostedWriteRuntime, sessionID string, d distilled) (int, error) {
	return withResolvedTarget(ctx, runtime.targets, runtime.targetInput, func(ctx context.Context, receipt targetContext) (int, error) {
		return postSessionDistill(ctx, runtime.client, runtime.apiURL, runtime.token, receipt.RepositoryWorkspaceID, sessionID, d)
	})
}

// sessionDistillEnabled asks the hosted API whether the session-end distiller is on
// for this deployment: GET {apiURL}/session-distill-enabled → {"enabled": bool}.
// This is the env-wide WorkOS flag lema-session-distill (ADR-0111/0140) surfaced to
// a client that has no WorkOS session of its own — it authenticates with its
// lema_live_ token, the same one it posts with. Fail-closed: false on any non-200
// or transport error, so the distiller stays dark unless the API affirmatively says
// it is on. Mirrors pushProducerEnabled.
func sessionDistillEnabled(ctx context.Context, client *http.Client, apiURL, token string) bool {
	if apiURL == "" || token == "" {
		return false
	}
	url := strings.TrimRight(apiURL, "/") + "/session-distill-enabled"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return false
	}
	return body.Enabled
}

// ---- scan + scrub + assemble the deliberation -----------------------------

// distillMaxChars bounds the assembled deliberation the client ships. The server
// truncates session sources near this (~48000 chars), so anything past it is
// dropped there anyway; bounding it here keeps the request small and means even a
// giant session never ships megabytes over the wire (ADR-0140).
const distillMaxChars = 48000

// scanTranscriptForDistill opens a transcript file and returns the scrubbed,
// assembled deliberation in it. A missing/unreadable file is an error the caller
// treats as fail-open. It runs only AFTER the gate has said the flag is on — the
// privacy-load-bearing ordering (nothing is opened while the distiller is dark).
func scanTranscriptForDistill(path string) (distilled, error) {
	f, err := os.Open(path)
	if err != nil {
		return distilled{}, err
	}
	defer f.Close()
	return distillDeliberation(f), nil
}

// distillDeliberation does the single, substring-gated pass over one transcript,
// assembling the user + assistant PROSE turns (the conversation/reasoning) into the
// deliberation text — each turn scrubbed of credential-shaped substrings, the whole
// payload bounded by distillMaxChars. It deliberately keeps only prose: a user
// tool_result-only turn and an assistant tool_use-only turn carry mechanical
// chatter the extractor is tuned to ignore, so they are skipped (ADR-0140). Reuses
// sessions.go's streamed reader (readScanLine / scannerBufCap) and scrubSecrets so
// a giant transcript stays O(kept-prose), not O(file), and adds no new machinery.
func distillDeliberation(r io.Reader) distilled {
	br := bufio.NewReaderSize(r, scannerBufCap)
	var turns []string
	total := 0
	shortestCwd := ""

	for {
		line, oversized, eof := readScanLine(br)
		if oversized {
			// One line exceeded scannerBufCap (a giant tool body); it was fully
			// drained. Skip just it and keep scanning, exactly like scanSession.
			if eof {
				break
			}
			continue
		}
		if eof && len(line) == 0 {
			break
		}
		if len(line) == 0 {
			if eof {
				break
			}
			continue
		}

		// Cheap cwd sample for the best-effort repo label — a direct substring read
		// (indexCwdValue), no full-record parse, exactly as scanSession does it.
		if bytesContains(line, `"cwd":"`) {
			if c := indexCwdValue(line); c != "" {
				if shortestCwd == "" || len(c) < len(shortestCwd) {
					shortestCwd = c
				}
			}
		}

		// Substring gate: only user/assistant records can carry prose; skip the rest
		// (mode/summary/title/tool bookkeeping lines) unparsed.
		isUser := bytesContains(line, `"type":"user"`)
		isAssistant := bytesContains(line, `"type":"assistant"`)
		if !isUser && !isAssistant {
			if eof {
				break
			}
			continue
		}

		if total < distillMaxChars {
			var rec jsonlRecord
			if json.Unmarshal(line, &rec) == nil {
				var prose, role string
				switch rec.Type {
				case "user":
					if !rec.IsMeta {
						prose, role = userProseTurn(rec.Message), "User"
					}
				case "assistant":
					prose, role = assistantProseTurn(rec.Message), "Assistant"
				}
				if prose != "" {
					turn := role + ": " + prose
					turns = append(turns, turn)
					total += len(turn) + 2 // + the "\n\n" joiner
				}
			}
			// A malformed line is tolerated (skipped), never fatal.
		}

		if eof {
			break
		}
	}

	repo := ""
	if shortestCwd != "" {
		repo = filepath.Base(shortestCwd)
	}
	// capLen bounds the assembled text on a rune boundary (chars ≈ the server's cap),
	// so a session that overruns is trimmed rather than shipped whole.
	return distilled{Text: capLen(strings.Join(turns, "\n\n"), distillMaxChars), Repo: repo}
}

// userProseTurn extracts a user turn's authored prose and scrubs it. It returns ""
// for turns that are NOT real user prose — tool_result-only turns (no text block)
// and system-injected envelopes whose text opens with an angle-bracket tag
// (<command-name>, <task-notification>, …) — mirroring sessions.go's userPromptText,
// but with the deliberation scrub (no per-turn 200-char cap: the reasoning must stay
// intact; the whole payload is bounded by distillMaxChars instead).
func userProseTurn(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var um userMessage
	if json.Unmarshal(raw, &um) != nil {
		return ""
	}
	text := strings.TrimSpace(extractContentText(um.Content))
	if text == "" {
		return "" // tool_result-only turn (no authored text)
	}
	if strings.HasPrefix(text, "<") {
		return "" // system-injected envelope, not deliberation
	}
	return scrubDeliberation(text)
}

// assistantProseTurn extracts an assistant turn's text blocks (its reasoning) and
// scrubs them. A tool_use-only assistant turn has no text block and yields "" — the
// mechanical chatter the extractor is tuned to ignore is dropped at the source.
func assistantProseTurn(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var am assistantMessage
	if json.Unmarshal(raw, &am) != nil {
		return ""
	}
	var parts []string
	for _, b := range am.Content {
		if b.Type == "text" {
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		}
	}
	if len(parts) == 0 {
		return "" // tool_use-only turn — no prose
	}
	return scrubDeliberation(strings.Join(parts, " "))
}

// scrubDeliberation is the privacy gate for deliberation text: collapse whitespace
// to a single line, then redact credential-shaped substrings (the same
// collapseWS + scrubSecrets sanitizePrompt applies). It deliberately omits
// sanitizePrompt's hard 200-char per-turn cap — a distiller needs the reasoning
// intact for extraction, and the whole payload is bounded by distillMaxChars — but
// the SECRET scrub is load-bearing: it is what keeps a pasted key/token from
// crossing the wire (ADR-0140).
func scrubDeliberation(s string) string {
	return strings.TrimSpace(scrubSecrets(collapseWS(s)))
}

// ---- the ingest-session client --------------------------------------------
//
// The wire contract mirrors the server's ingestSessionRequest / -Response
// (apps/api/internal/api/ingest_session.go). It is duplicated here rather than
// imported because the client owns its DTO and must not pull the server package
// into the public binary; the shape is pinned by the round-trip test.

type distillRequest struct {
	SessionID  string `json:"session_id"`
	Transcript string `json:"transcript"`
	Repo       string `json:"repo,omitempty"`
}

type distillResponse struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Claims    int    `json:"claims"`
	Reason    string `json:"reason,omitempty"`
}

// postSessionDistill POSTs the scrubbed deliberation to the workspace ingest-session
// endpoint as the authed programmatic principal (Bearer, like pushDecisions). The
// server extracts and mints `proposed` atoms (never bound — the trust tier, ADR-0140
// / 0125). Returns the number of atoms harvested. A non-2xx (including the dark 404
// when the flag is off server-side) is an error the caller swallows (fail-open for
// the hook). Empty transcript is a no-op — nothing to send.
func postSessionDistill(ctx context.Context, client *http.Client, apiURL, token, workspaceID, sessionID string, d distilled) (int, error) {
	if strings.TrimSpace(d.Text) == "" {
		return 0, nil
	}
	body, err := json.Marshal(distillRequest{
		SessionID:  sessionID,
		Transcript: d.Text,
		Repo:       d.Repo,
	})
	if err != nil {
		return 0, err
	}
	url := strings.TrimRight(apiURL, "/") + "/workspaces/" + workspaceID + "/ingest-session"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("ingest-session: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out distillResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return 0, fmt.Errorf("ingest-session: decode response: %w", err)
	}
	return out.Claims, nil
}
