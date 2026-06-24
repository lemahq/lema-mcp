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

	"github.com/lemahq/lema-mcp/internal/verdict"
)

// push.go is the `lema-mcp push` subcommand — the PRODUCER half of the
// self-improving loop (ADR-0124 Phase 4; the automate-the-draft-not-the-judgment
// rationale, and why a Stop hook over an explicit /archive command, is ADR-0129).
// It runs as a Stop hook: at the moment
// an agent ends its turn it scans the session transcript for the deterministic
// Signal-A pattern — a check_approach that found NO recorded ruling, followed by
// a code edit that ADOPTS the checked approach — and drafts that adoption to the
// operator's hosted workspace as a `proposed` decision. A human's later in-app
// accept is what promotes + binds it (ADR-0125); the push itself can only ever
// draft, never bind, so the risk is noise, not poison.
//
// Two structural facts this implements (both verified, see
// workspace/.../mcp-end-state/phase4-write-loop-design.md):
//   - There was never a push.go / feat/push branch; the client is authored here.
//     The server side (POST /workspaces/{id}/import-decisions, programmatic →
//     coerced to proposed) is already merged.
//   - Signal A rides the TRANSCRIPT (a Stop hook), not the MCP server: an MCP
//     server sees its own tool calls but never the host's Edit/Write, so the
//     check→no-ruling→edit pattern is only observable in the transcript. This
//     mirrors the live capture-guard.py Stop hook and reuses the same Go
//     transcript machinery (sessions.go / capture_rate.go).
//
// The whole feature is dark behind LEMA_FUSE_PUSH (default OFF), so it ships
// silent in prod until the flag is set (mirrors the backend
// settledVerdictEnabled).

const (
	pushFlagEnv      = "LEMA_FUSE_PUSH"    // master switch, default OFF (dark in prod)
	pushWorkspaceEnv = "LEMA_WORKSPACE_ID" // the workspace the drafts land in
)

// pushEnabled reports whether the push producer is on. Default OFF, read per
// invocation — the flag-flip-not-a-build lever, mirroring the backend
// settledVerdictEnabled.
func pushEnabled() bool {
	v := os.Getenv(pushFlagEnv)
	return v == "1" || v == "true"
}

// stopHookInput is the subset of the Claude Code Stop-hook stdin payload the
// producer reads (mirrors capture-guard.py). Other fields are ignored.
type stopHookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	StopHookActive bool   `json:"stop_hook_active"`
}

// pushRunner is runPush's testable core with the I/O seams injected (transcript
// scan, the HTTP push, the clock, and whether credentials resolved). The shell
// runPush wires the real implementations; tests pass fakes.
type pushRunner struct {
	scan    func(path string) ([]pushCandidate, error)
	push    func(ctx context.Context, records []pushRecord) (pushResponse, error)
	now     func() time.Time
	canPush bool // credentials + workspace resolved
}

// run executes the Signal-A producer for one Stop event and returns the number of
// drafts pushed (0 for any no-op). Every failure path is a fail-open no-op — a
// capture hook must never wedge a session — and it NEVER blocks the stop or nags:
// it drafts silently, leaving the human's accept as the only judgment.
func (r pushRunner) run(ctx context.Context, in stopHookInput) int {
	if in.StopHookActive || strings.TrimSpace(in.TranscriptPath) == "" {
		return 0 // re-entrant stop, or nothing to read
	}
	if !r.canPush {
		return 0 // no credentials/workspace resolved — nowhere to draft; fail-open
	}
	cands, err := r.scan(in.TranscriptPath)
	if err != nil || len(cands) == 0 {
		return 0
	}
	resp, err := r.push(ctx, candidateRecords(cands, r.now()))
	if err != nil {
		return 0 // a push failure must not wedge the session
	}
	return resp.Created + resp.Updated
}

// runPush is the `lema-mcp push` Stop-hook body — the thin I/O shell over the
// tested pushRunner. Dark unless LEMA_FUSE_PUSH is set. It reads the Stop payload
// from stdin, resolves the hosted credentials + target workspace
// (LEMA_WORKSPACE_ID), and drafts any Signal-A adoptions as proposed. Always
// returns (exit 0) and never writes a block decision to stdout: a producer
// failure must never wedge a session, and the hook drafts silently rather than
// nagging (the human's in-app accept is the only judgment).
//
// Wire it as a Stop hook in .claude/settings.json once the flag is on:
//
//	"Stop": [{ "matcher": "", "hooks": [{ "type": "command",
//	  "command": "lema-mcp push" }]}]
func runPush(args []string) {
	if !pushEnabled() {
		return
	}
	data, ok := readStopStdin(3 * time.Second)
	if !ok {
		return
	}
	var in stopHookInput
	if json.Unmarshal(data, &in) != nil {
		return
	}
	apiURL, token, _ := resolveHostedConfig()
	workspaceID := strings.TrimSpace(os.Getenv(pushWorkspaceEnv))
	// Bound the push so a slow/hung API can never delay the agent's turn-end (the
	// stdin read is already bounded; the network call must be too).
	client := &http.Client{Timeout: pushTimeout}
	r := pushRunner{
		scan: scanTranscriptForCandidates,
		push: func(ctx context.Context, records []pushRecord) (pushResponse, error) {
			return pushDecisions(ctx, client, apiURL, token, workspaceID, records)
		},
		now:     time.Now,
		canPush: apiURL != "" && token != "" && workspaceID != "",
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	if n := r.run(ctx, in); n > 0 {
		fmt.Fprintf(os.Stderr, "lema-mcp push: drafted %d decision(s) as proposed — accept in-app to confirm and record them\n", n)
	}
}

// pushTimeout bounds the whole push (the HTTP client and the request context) so
// a Stop hook can never hang the agent's turn-end on a slow API.
const pushTimeout = 10 * time.Second

// readStopStdin reads the hook payload from stdin with a timeout so an invocation
// without a piped payload never hangs the agent. ok=false on timeout or read
// error (fail-open). Mirrors runGuard's stdin handling.
func readStopStdin(timeout time.Duration) ([]byte, bool) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(os.Stdin)
		ch <- result{data, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, false
		}
		return r.data, true
	case <-time.After(timeout):
		return nil, false
	}
}

// eventKind tags the transcript events Signal A cares about.
type eventKind int

const (
	evCheckCall   eventKind = iota // a check_approach tool_use (carries the approach queried)
	evCheckResult                  // its tool_result (carries the verdict)
	evEdit                         // an Edit/Write/MultiEdit tool_use (carries the edit text + files)
)

// transcriptEvent is one ordered, relevant event distilled from a session
// transcript. Only the handful of fields Signal A needs are kept; everything
// else in the raw record is dropped.
type transcriptEvent struct {
	kind      eventKind
	toolUseID string   // correlates an evCheckCall with its evCheckResult
	approach  string   // evCheckCall: the check_approach `approach` input
	verdict   string   // evCheckResult: the check_approach verdict
	editText  string   // evEdit: guardQuery text (file base + new text) for scope matching
	refs      []string // evEdit: the file paths the edit touched
}

// pushCandidate is a low-richness draft Signal A produces deterministically (no
// model): an approach the agent adopted after check_approach found no prior
// ruling. There is no rejected alternative — the deterministic signal cannot see
// the counterfactual (that is Signal B's job, gated separately).
type pushCandidate struct {
	Approach string   // the adopted approach (the decision's chosen + title)
	Refs     []string // the files the adopting edit touched
}

// detectCandidates is the deterministic heart of Signal A: walk the ordered
// transcript events and, for every check_approach that returned no_recorded_ruling
// then was followed by an edit adopting that approach, emit one candidate. Pure
// and order-sensitive — an edit only adopts an approach whose no-ruling result
// was already seen — so the same transcript always yields the same drafts (no
// model). At most one candidate per approach (the first adopting edit consumes it).
func detectCandidates(events []transcriptEvent) []pushCandidate {
	pending := map[string]string{} // toolUseID -> approach (check seen, result pending)
	var eligible []string          // approach keys with a no_recorded_ruling result, in check-result order
	approachText := map[string]string{}
	eligibleSeen := map[string]bool{} // dedup the eligible slice
	consumed := map[string]bool{}     // approach key -> already drafted
	var out []pushCandidate

	for _, e := range events {
		switch e.kind {
		case evCheckCall:
			if e.approach != "" {
				pending[e.toolUseID] = e.approach
			}
		case evCheckResult:
			if ap, ok := pending[e.toolUseID]; ok {
				if e.verdict == verdictNoRuling {
					key := approachKey(ap)
					if !eligibleSeen[key] {
						eligibleSeen[key] = true
						eligible = append(eligible, key)
						approachText[key] = ap
					}
				}
				delete(pending, e.toolUseID)
			}
		case evEdit:
			if e.editText == "" {
				continue
			}
			editTokens := tokenSet(e.editText)
			// Iterate eligibility order (not a map) so the drafts are stable.
			for _, key := range eligible {
				if consumed[key] {
					continue
				}
				if approachAdopted(approachText[key], editTokens) {
					out = append(out, pushCandidate{Approach: approachText[key], Refs: e.refs})
					consumed[key] = true
				}
			}
		}
	}
	return out
}

// approachKey normalizes an approach to a dedup key.
func approachKey(approach string) string {
	return strings.ToLower(strings.TrimSpace(approach))
}

// approachStopwords are high-frequency words stripped from an approach phrase
// before scope-matching, so the match keys on the approach's DISTINCTIVE terms.
// verdict.significantTokens is the canonical list but is unexported across the
// internal boundary; this is the minimal subset that shows up in approach prose
// (verdict.Tokenize already drops sub-2-rune tokens, and we additionally require
// len >= 3 below, so short function words like "to"/"of" never reach this set).
var approachStopwords = map[string]bool{
	"the": true, "for": true, "and": true, "with": true, "use": true,
	"using": true, "our": true, "this": true, "that": true, "from": true,
	"into": true, "via": true, "are": true, "how": true, "should": true,
	"add": true, "support": true, "new": true, "instead": true,
}

// approachAdopted reports whether an edit's text adopts `approach`: either the
// approach's joined alphanumeric form appears as one token (handles "PostgreSQL",
// "gRPC", "spring-boot"), or every DISTINCTIVE term of the approach appears as a
// whole token in the edit. All-terms-present is high precision (the edit really
// is about the approach) and low recall (a paraphrase misses) — the deterministic
// Signal-A contract. Reuses the guard's tokenSet/alnumLower so the scope match is
// the same shape as the never-reopen guard's.
func approachAdopted(approach string, editTokens map[string]bool) bool {
	if len(editTokens) == 0 {
		return false
	}
	if j := alnumLower(approach); len(j) >= 3 && editTokens[j] {
		return true
	}
	seen := map[string]bool{}
	var sig []string
	for _, t := range verdict.Tokenize(approach) {
		if len(t) < 3 || approachStopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		sig = append(sig, t)
	}
	if len(sig) == 0 {
		return false
	}
	for _, t := range sig {
		if !editTokens[t] {
			return false
		}
	}
	return true
}

// parseTranscriptEvents reads a Claude Code session transcript (JSONL) and
// distills the ordered events Signal A cares about: check_approach tool_use
// calls, their tool_result verdicts, and Edit/Write/MultiEdit tool_use calls.
// Streamed and substring-gated exactly like the Sessions scan (sessions.go) so a
// giant transcript stays O(events), not O(file). Tolerant: an unparseable line
// is skipped, never fatal.
func parseTranscriptEvents(r io.Reader) ([]transcriptEvent, error) {
	br := bufio.NewReaderSize(r, scannerBufCap)
	var events []transcriptEvent
	for {
		line, oversized, eof := readScanLine(br)
		if oversized {
			if eof {
				break
			}
			continue // a giant body we never need to parse; skip and keep scanning
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

		// Cheap substring gate (mirrors sessions.go): only tool_use / tool_result
		// lines matter; the bulk text bodies are skipped unparsed.
		if !bytesContains(line, `"type":"tool_use"`) && !bytesContains(line, `"type":"tool_result"`) {
			if eof {
				break
			}
			continue
		}
		var rec jsonlRecord
		if json.Unmarshal(line, &rec) != nil {
			if eof {
				break
			}
			continue // tolerate a malformed line
		}

		switch rec.Type {
		case "assistant":
			for _, b := range messageBlocks(rec.Message) {
				if b.Type != "tool_use" {
					continue
				}
				switch {
				case isCheckApproachTool(b.Name):
					if ap := checkApproachInputApproach(b.Input); ap != "" {
						events = append(events, transcriptEvent{kind: evCheckCall, toolUseID: b.ID, approach: ap})
					}
				case isEditTool(b.Name):
					if text, refs := editTextAndRefs(b.Input); text != "" {
						events = append(events, transcriptEvent{kind: evEdit, editText: text, refs: refs})
					}
				}
			}
		case "user":
			for _, b := range messageBlocks(rec.Message) {
				if b.Type != "tool_result" {
					continue
				}
				if v := verdictFromResult(b, rec.ToolUseResult); v != "" {
					events = append(events, transcriptEvent{kind: evCheckResult, toolUseID: b.ToolUseID, verdict: v})
				}
			}
		}

		if eof {
			break
		}
	}
	return events, nil
}

// scanTranscriptForCandidates opens a transcript file and returns the Signal-A
// candidates in it (parse + detect). A missing/unreadable file is an error the
// caller treats as fail-open.
func scanTranscriptForCandidates(path string) ([]pushCandidate, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	events, err := parseTranscriptEvents(f)
	if err != nil {
		return nil, err
	}
	return detectCandidates(events), nil
}

// pushMessage / pushBlock are push-local decodings of a transcript record's
// message. They duplicate (rather than extend) sessions.go's contentBlock on
// purpose: Signal A needs the tool_use `id`, the tool_result `tool_use_id`, and
// the tool_result `content` — none of which contentBlock carries — and keeping
// the decoding here means push.go touches no shared file (the reseed lane owns
// the rest of this tree).
type pushMessage struct {
	Content json.RawMessage `json:"content"` // a JSON string (prose turn) OR a []pushBlock
}

type pushBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`          // tool_use id
	Name      string          `json:"name"`        // tool_use name
	Input     json.RawMessage `json:"input"`       // tool_use input
	ToolUseID string          `json:"tool_use_id"` // tool_result -> tool_use correlation
	Content   json.RawMessage `json:"content"`     // tool_result content (JSON string OR []block)
	Text      string          `json:"text"`        // a text block's text
}

// messageBlocks decodes a record's message.content into blocks. Assistant turns
// and tool_result-bearing user turns carry a list of blocks; a plain user-prose
// turn is a bare string and yields no blocks.
func messageBlocks(raw json.RawMessage) []pushBlock {
	if len(raw) == 0 {
		return nil
	}
	var m pushMessage
	if json.Unmarshal(raw, &m) != nil || len(m.Content) == 0 {
		return nil
	}
	var blocks []pushBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		return nil // string content (no blocks)
	}
	return blocks
}

var editToolNames = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// toolSuffix strips an optional mcp__<server>__ prefix so any server alias of a
// tool resolves to its bare name (mirrors isRecordDecisionTool in sessions.go).
func toolSuffix(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndex(name, "__"); i >= 0 {
		return name[i+2:]
	}
	return name
}

func isCheckApproachTool(name string) bool { return toolSuffix(name) == "check_approach" }
func isEditTool(name string) bool          { return editToolNames[toolSuffix(name)] }

// checkApproachInputApproach pulls the `approach` field from a check_approach
// tool_use input (checkApproachInput = {repo, approach}).
func checkApproachInputApproach(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var in struct {
		Approach string `json:"approach"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return ""
	}
	return strings.TrimSpace(in.Approach)
}

// docFileExts are non-code, prose file types. An edit to one is documentation
// ABOUT an approach (an ADR, notes, a docs scenario), not an adoption of it in
// code — real-data scanning showed doc edits were the dominant false positive.
// Writing an approach into an ADR/doc is the EXPLICIT capture path; Signal A
// targets the silent code-adoption gap.
var docFileExts = map[string]bool{
	".md": true, ".mdx": true, ".markdown": true, ".txt": true, ".rst": true,
}

func isDocFile(path string) bool {
	return docFileExts[strings.ToLower(filepath.Ext(path))]
}

// editTextAndRefs reuses the guard's guardQuery to assemble the scope-match text
// (file base + the new text the tool writes) and pulls file_path for the refs.
// A doc-text edit yields ("", nil) so it is not treated as a code adoption.
func editTextAndRefs(raw json.RawMessage) (string, []string) {
	if len(raw) == 0 {
		return "", nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return "", nil
	}
	if p, ok := m["file_path"].(string); ok && isDocFile(p) {
		return "", nil // prose about an approach is not adoption of it
	}
	text := guardQuery(m)
	var refs []string
	if p, ok := m["file_path"].(string); ok && strings.TrimSpace(p) != "" {
		refs = []string{p}
	}
	return text, refs
}

// verdictFromResult extracts the check_approach verdict from a tool_result. The
// content is observed as a JSON string carrying the serialized checkApproachOutput;
// a list of text blocks, a structured object, or the record-level toolUseResult
// string are all tolerated. "" for any non-check tool_result (no verdict field),
// so the caller emits no event for it.
func verdictFromResult(b pushBlock, toolUseResult json.RawMessage) string {
	for _, raw := range []json.RawMessage{b.Content, toolUseResult} {
		if v := verdictFromJSONish(raw); v != "" {
			return v
		}
	}
	return ""
}

func verdictFromJSONish(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// A JSON string that itself holds the JSON object (the observed shape).
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return verdictField(s)
	}
	// A list of content blocks ([{type:text,text:"{...}"}]).
	var blocks []pushBlock
	if json.Unmarshal(raw, &blocks) == nil {
		for _, b := range blocks {
			if v := verdictField(b.Text); v != "" {
				return v
			}
		}
		return ""
	}
	// A structured object directly.
	return verdictField(string(raw))
}

// verdictField returns the "verdict" field of a JSON object encoded in jsonText,
// or "" if it is not a JSON object with that field.
func verdictField(jsonText string) string {
	t := strings.TrimSpace(jsonText)
	if t == "" {
		return ""
	}
	var v struct {
		Verdict string `json:"verdict"`
	}
	if json.Unmarshal([]byte(t), &v) == nil {
		return strings.TrimSpace(v.Verdict)
	}
	return ""
}

// ---- the push client (C1) -------------------------------------------------
//
// The wire contract mirrors the server's importDecisionsRequest / -Record
// (apps/api/internal/api/import_decisions.go). It is duplicated here rather than
// imported because the client owns its DTO and must not pull the server package
// into the public binary; the shape is pinned by the round-trip test.

const (
	pushSchemaVersion  = 1 // import_decisions.go rejects anything but 1
	pushStatusProposed = "proposed"
	pushActorName      = "lema-mcp push (Signal A)"
	// pushRationale is the honest, fixed rationale for a deterministic Signal-A
	// draft: it states only what the transcript proves (the approach was adopted
	// after check_approach found no ruling), invents no why, and points the human
	// at the confirm step. The draft lands `proposed` and binds only on an
	// interactive accept (ADR-0125), so it can add noise, never poison.
	pushRationale = "Adopted while implementing; check_approach found no recorded ruling for this approach. " +
		"Drafted automatically by lema-mcp (Signal A) — review and accept in-app to confirm and record it."
)

type pushRejectedAlt struct {
	Option string `json:"option"`
	Why    string `json:"why,omitempty"`
}

type pushRecord struct {
	ID        string            `json:"id"`
	TS        string            `json:"ts"`
	Title     string            `json:"title"`
	Chosen    string            `json:"chosen"`
	Rejected  []pushRejectedAlt `json:"rejected,omitempty"`
	Rationale string            `json:"rationale,omitempty"`
	Refs      []string          `json:"refs,omitempty"`
	Status    string            `json:"status"`
}

type pushRequest struct {
	SchemaVersion int          `json:"schema_version"`
	ActorName     string       `json:"actor_name,omitempty"`
	DryRun        bool         `json:"dry_run,omitempty"`
	Records       []pushRecord `json:"records"`
}

type pushResult struct {
	LocalID    string  `json:"local_id"`
	Title      string  `json:"title"`
	Status     string  `json:"status"`
	Reason     string  `json:"reason,omitempty"`
	DecisionID *string `json:"decision_id,omitempty"`
}

type pushResponse struct {
	Created int  `json:"created"`
	Updated int  `json:"updated"`
	Skipped int  `json:"skipped"`
	Failed  int  `json:"failed"`
	DryRun  bool `json:"dry_run,omitempty"`
	// RecordedBy is server-derived ("agent" for a programmatic push). The client
	// renders it honestly — "recorded as agent; accept in-app to bind" — and never
	// claims the push bound anything.
	RecordedBy string       `json:"recorded_by"`
	Results    []pushResult `json:"results"`
}

// candidateRecords maps Signal-A candidates to wire records, stamped at `now`.
// Status is always proposed and there is never a rejected alternative — the
// deterministic signal cannot see the counterfactual (that is Signal B). The id
// is content-keyed (sessionDecisionID) so re-running the same session does not
// duplicate the draft.
func candidateRecords(cands []pushCandidate, now time.Time) []pushRecord {
	ts := now.UTC().Format(time.RFC3339)
	out := make([]pushRecord, 0, len(cands))
	for _, c := range cands {
		title := trimTitle(c.Approach)
		out = append(out, pushRecord{
			ID:        sessionDecisionID(title, c.Approach),
			TS:        ts,
			Title:     title,
			Chosen:    c.Approach,
			Rationale: pushRationale,
			Refs:      c.Refs,
			Status:    pushStatusProposed,
		})
	}
	return out
}

// pushDecisions POSTs records to the workspace import endpoint as the authed
// programmatic principal. The server coerces a programmatic push to `proposed`
// regardless of the status sent, so this can only ever DRAFT. Returns the
// server's summary (incl. the server-derived recorded_by). A non-2xx is an error
// (fail loud) — the caller decides to swallow it (fail-open for the hook).
func pushDecisions(ctx context.Context, client *http.Client, apiURL, token, workspaceID string, records []pushRecord) (pushResponse, error) {
	if len(records) == 0 {
		return pushResponse{}, nil
	}
	body, err := json.Marshal(pushRequest{
		SchemaVersion: pushSchemaVersion,
		ActorName:     pushActorName,
		Records:       records,
	})
	if err != nil {
		return pushResponse{}, err
	}
	url := strings.TrimRight(apiURL, "/") + "/workspaces/" + workspaceID + "/import-decisions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return pushResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return pushResponse{}, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pushResponse{}, fmt.Errorf("import-decisions: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var out pushResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return pushResponse{}, fmt.Errorf("import-decisions: decode response: %w", err)
	}
	return out, nil
}
