package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/lemahq/lema-mcp/internal/adr"
	"github.com/lemahq/lema-mcp/internal/decisioncheck"
	"github.com/lemahq/lema-mcp/internal/source"
	"github.com/lemahq/lema-mcp/internal/verdict"
)

// guardInput is the subset of the Claude Code PreToolUse stdin payload the guard
// reads. Other fields (session_id, cwd, …) are ignored. See ADR-0052.
type guardInput struct {
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
}

// guardOutput / hookSpecificOutput are the PreToolUse response contract.
// permissionDecision is "ask" (a human prompt) or OMITTED (a context-mode nudge):
// the guard never emits "allow" — that would skip the user's normal Edit/Write
// confirmation on the very edit it is flagging. The autonomous matcher never emits
// "deny" either; the ONLY deny is human-bound — an attended :respect resolution in
// terminal mode (see guard_terminal.go), never the matcher on its own.
// additionalContext is the non-blocking agent nudge; permissionDecisionReason is
// the "ask"/deny reason (ADR-0052).
type guardOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}
type hookSpecificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

const guardMaxQuery = 4000

// guardQuery assembles the text the guard matches against killed options from a
// tool's input: the file path base plus the NEW text a tool writes — Edit's
// new_string, Write's content, MultiEdit's edits[].new_string, and a Bash command.
// It deliberately ignores old_string (the removed text) and reads only fields
// Claude Code actually emits — Edit is {file_path, old_string, new_string}, Write
// is {file_path, content}, MultiEdit is {file_path, edits[]} (ADR-0052).
func guardQuery(in map[string]any) string {
	var parts []string
	if p, ok := in["file_path"].(string); ok && p != "" {
		parts = append(parts, filepath.Base(p))
	}
	for _, k := range []string{"new_string", "content", "command"} {
		if v, ok := in[k].(string); ok && v != "" {
			parts = append(parts, v)
		}
	}
	if edits, ok := in["edits"].([]any); ok {
		for _, e := range edits {
			if m, ok := e.(map[string]any); ok {
				if v, ok := m["new_string"].(string); ok && v != "" {
					parts = append(parts, v)
				}
			}
		}
	}
	// Newline-joined so each part starts its own line: a citation marker in one
	// part cannot exempt another part's text, and the basename stays matchable
	// even when the first text line is a citation line (stripCitationLines).
	q := strings.Join(parts, "\n")
	if len(q) > guardMaxQuery {
		q = q[:guardMaxQuery]
	}
	return q
}

// guardCitationRE marks a line as CITING a prior ruling rather than proposing:
// rejection vocabulary (reject/rejected/rejecting/rejects, "rejected_alternative",
// "ruled out", "supersede…", the "killed:" list idiom — boundary-anchored so
// "skilled:" is not a marker) or an explicit decision ref (ADR-N…, d_<hex>).
// Bare hex ids are deliberately NOT markers — they collide with commit hashes,
// which appear on nearly every HANDOFF line and would exempt that text wholesale.
// The d_ ref is case-sensitive (ids are minted lowercase, d_%06x) and requires
// ≥6 hex chars, so hex-spelling identifiers like d_added/d_beef don't qualify
// (an all-letter 6-hex word like d_decade remains a rare accepted residual).
// Bare "killed" is not a marker either (process-kill prose); only the colon
// list form counts.
var guardCitationRE = regexp.MustCompile(`(?i)\breject(?:ed|ing|s)?\b|rejected_alternatives?|\bruled[ -]?out\b|\bsupersed\w*\b|\bkilled:|\badr-?\d{1,5}\b|(?-i:\bd_[0-9a-f]{6,}\b)`)

// stripCitationLines returns s minus the lines that cite a prior ruling, plus
// whether anything was stripped. The guard exists to surface an UNKNOWN
// rejection; a line that names the rejection or cites the ruling is already the
// surfacing behavior lema wants, so its text does not count as proposal text.
// Line-scoped on purpose: a genuine re-proposal elsewhere in the same edit still
// fires (pain-point #4; ADR-0052). A citation is PROSE: a line with no interior
// whitespace (a bare basename like adr-0140-notes.go, a lone identifier) cannot
// be citing anything, so a marker inside it never strips it — the basename line
// guardQuery emits stays matchable.
func stripCitationLines(s string) (string, bool) {
	if !guardCitationRE.MatchString(s) {
		return s, false
	}
	var kept []string
	stripped := false
	for _, ln := range strings.Split(s, "\n") {
		if strings.ContainsAny(strings.TrimSpace(ln), " \t") && guardCitationRE.MatchString(ln) {
			stripped = true
			continue
		}
		kept = append(kept, ln)
	}
	return strings.Join(kept, "\n"), stripped
}

func tokenSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, t := range verdict.Tokenize(s) {
		m[t] = true
	}
	return m
}

// alnumLower returns s lowercased with every non-alphanumeric rune removed, e.g.
// "PostgreSQL" -> "postgresql" — the joined form used to match an option written
// without its internal punctuation.
func alnumLower(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// optionMatches reports whether the killed option `key` is present in the edit's
// token set, and a specificity score (the option's joined length) when it is. A
// match needs EITHER the joined option form as a whole token (handles
// "postgresql") OR every camelCase/punctuation piece of the option present as a
// whole token (handles "PostgreSQL" / "spring-boot"). It is whole-token, so a
// killed option is never matched as a substring of a larger word, and it reads the
// option NAME only — an edit echoing a decision's rationale prose does not fire
// (ADR-0052).
func optionMatches(key string, edit map[string]bool) (bool, float64) {
	joined := alnumLower(key)
	if len(joined) < 3 {
		return false, 0 // too short/trivial to match safely
	}
	if edit[joined] {
		return true, float64(len(joined))
	}
	pieces := verdict.Tokenize(key)
	if len(pieces) == 0 {
		return false, 0
	}
	for _, p := range pieces {
		if !edit[p] {
			return false, 0
		}
	}
	return true, float64(len(joined))
}

// matchClosed returns the CLOSED atoms whose killed option appears in the edit,
// most-specific first, with Score set to the match specificity. It reads the
// option from atom.MatchKey (the option name / superseded choice), never the
// rationale prose (ADR-0052). Per-atom token-set choice: an option whose NAME
// itself carries a citation marker (e.g. "supersedes queue") can never sit on a
// non-citation line, so the exemption structurally cannot apply to it — when
// `full` is non-nil such atoms match against the full query instead of the
// citation-stripped one (a false nudge beats a permanently silent guard).
func matchClosed(closed []source.Atom, edit, full map[string]bool) []source.Atom {
	var out []source.Atom
	for _, a := range closed {
		if a.MatchKey == "" {
			continue
		}
		set := edit
		if full != nil && guardCitationRE.MatchString(a.MatchKey) {
			set = full
		}
		if len(set) == 0 {
			continue
		}
		if ok, score := optionMatches(a.MatchKey, set); ok {
			a.Score = score
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// guardMatch matches the killed options against one flat text (no citation
// semantics) — the primitive matchClosed wraps.
func guardMatch(closed []source.Atom, editText string) []source.Atom {
	return matchClosed(closed, tokenSet(editText), nil)
}

// guardFires is THE fire floor, shared by the fire decision and the suppression
// counterfactual so the two can never drift apart.
func guardFires(hits []source.Atom) bool {
	return len(hits) > 0 && hits[0].Score >= guardMinScore
}

// evaluateCitation is the single citation-aware evaluation of one edit query:
// hits is what the guard fires on (killed options on non-citation lines, plus
// marker-named options against the full query), and suppressed is the
// calibration counterfactual — the top atom whose fire ONLY the citation
// exemption prevented (nil when the guard fired or nothing was stripped). Both
// callers (the PreToolUse hook and the terminal sidecar) consume this one
// evaluation, so fire and suppression can never disagree.
func evaluateCitation(closed []source.Atom, query string) (hits []source.Atom, suppressed *source.Atom) {
	kept, stripped := stripCitationLines(query)
	if !stripped {
		return guardMatch(closed, kept), nil
	}
	fullSet := tokenSet(query)
	hits = matchClosed(closed, tokenSet(kept), fullSet)
	if guardFires(hits) {
		return hits, nil
	}
	if full := matchClosed(closed, fullSet, fullSet); guardFires(full) {
		suppressed = &full[0]
	}
	return hits, suppressed
}

const (
	guardModeEnvVar  = "LEMA_GUARD_MODE"
	guardModeContext = "context" // default: non-blocking agent nudge (no permission change)
	guardModeAsk     = "ask"     // prompt the human on a strong match (opt-in; the demo mode)
	guardModeOff     = "off"     // kill switch

	// Score floors (the option's joined length). Calibration starting points,
	// tuned via LEMA_GUARD_LOG before any v2 hard block (ADR-0052).
	guardMinScore    = 3.0 // context-nudge floor: drop 2-char-token noise
	guardAskMinScore = 5.0 // ask-prompt floor: only a specific option interrupts a human
)

// evaluateGuard decides what the PreToolUse hook returns for one edit, plus the
// matched CLOSED atom (for the calibration log). nil,nil = allow silently (no
// match, or too weak). A strong hit in ask mode prompts the human; everything else
// above the floor nudges the agent via additionalContext with NO permissionDecision
// — so the user's normal Edit/Write confirmation is never skipped. Only real CLOSED
// atoms (with their stored note) are surfaced; the guard never invents a why-not
// (ADR-0052).
func evaluateGuard(closed []source.Atom, query, mode string) (*guardOutput, *source.Atom) {
	// Citation exemption (pain-point #4): lines that cite a prior ruling do not
	// count as proposal text — see evaluateCitation/stripCitationLines.
	hits, _ := evaluateCitation(closed, query)
	return guardDecision(hits, mode)
}

// guardDecision maps one evaluation's hits to the PreToolUse response for the
// given mode — the shared tail of the hook and sidecar paths.
func guardDecision(hits []source.Atom, mode string) (*guardOutput, *source.Atom) {
	if mode == guardModeOff {
		return nil, nil
	}
	if !guardFires(hits) {
		return nil, nil
	}
	top := hits[0]
	reason := top.ClosedNote
	if reason == "" {
		reason = "this option was already ruled out: " + top.Text
	}
	if mode == guardModeAsk && top.Score >= guardAskMinScore {
		return &guardOutput{HookSpecificOutput: hookSpecificOutput{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "ask",
			PermissionDecisionReason: "lema — you're reaching for a decision your team already settled. " + reason,
		}}, &top
	}
	return &guardOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName: "PreToolUse",
		AdditionalContext: "lema never-reopen — this change reaches for a settled decision: " + reason +
			" If you are intentionally superseding it, call record_decision with supersedes; otherwise surface the prior decision instead of re-proposing it.",
	}}, &top
}

// citationExemptAtom returns the atom whose fire the citation exemption
// suppressed for this query — evaluateCitation's counterfactual — or nil.
// Calibration-only (guardLog): the exemption itself is measured, not guessed,
// before it is trusted (ADR-0052). Only FULLY suppressed fires are reported: if
// the kept lines still fire (even on a different atom, or downgraded
// ask→context), nothing is logged.
func citationExemptAtom(closed []source.Atom, query string) *source.Atom {
	_, suppressed := evaluateCitation(closed, query)
	return suppressed
}

// changeFromToolInput lifts a decisioncheck.Change from the PreToolUse payload:
// the target path plus the NEW text a tool writes (Edit new_string / Write
// content / MultiEdit edits[].new_string / a Bash command). It reads the raw
// file_path (decisioncheck rules path-scope, e.g. to launch-mode.ts), unlike
// guardQuery which reduces it to a basename for lexical matching.
func changeFromToolInput(in map[string]any) decisioncheck.Change {
	path, _ := in["file_path"].(string)
	var parts []string
	for _, k := range []string{"new_string", "content", "command"} {
		if v, ok := in[k].(string); ok && v != "" {
			parts = append(parts, v)
		}
	}
	if edits, ok := in["edits"].([]any); ok {
		for _, e := range edits {
			if m, ok := e.(map[string]any); ok {
				if v, ok := m["new_string"].(string); ok && v != "" {
					parts = append(parts, v)
				}
			}
		}
	}
	return decisioncheck.Change{Path: path, NewText: strings.Join(parts, "\n")}
}

// decisionCheckGuard runs the deterministic tier-1 rules over an edit and, on a
// hit, returns the PreToolUse response — an "ask" prompt in ask mode, else a
// non-blocking context nudge — citing the decision the change contradicts.
// nil = no rule fired (allow silently). Mirrors evaluateGuard's voice/shape.
func decisionCheckGuard(in map[string]any, mode string) *guardOutput {
	findings := decisioncheck.Check(changeFromToolInput(in))
	if len(findings) == 0 {
		return nil
	}
	f := findings[0]
	if mode == guardModeAsk {
		return &guardOutput{HookSpecificOutput: hookSpecificOutput{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "ask",
			PermissionDecisionReason: "lema — this change contradicts " + f.Cite + ". " + f.Message,
		}}
	}
	return &guardOutput{HookSpecificOutput: hookSpecificOutput{
		HookEventName: "PreToolUse",
		AdditionalContext: "lema decision-check — this change contradicts " + f.Cite + ". " + f.Message +
			" If you are intentionally superseding it, call record_decision with supersedes; otherwise follow the recorded decision.",
	}}
}

// guardADRPattern matches ADR filenames (NNNN-*.md / NNN_*.md) — the server's
// default; the guard parses the repo's ADRs per-edit for never-reopen (ADR-0053).
var guardADRPattern = regexp.MustCompile(`^\d{3,4}[-_].+\.md$`)

// loadADRClosed parses the repo's ADRs under dir and returns their CLOSED no-go
// atoms — accepted, non-superseded rejected-alternatives (ADR-0053). Fail-open:
// any discovery/parse error yields nil, so the guard never blocks on it.
func loadADRClosed(dir string) []source.Atom {
	adrDir := discoverLocalADRDir(dir)
	if adrDir == "" {
		return nil
	}
	adrs, err := adr.ParseDirMatching(adrDir, guardADRPattern)
	if err != nil {
		return nil
	}
	return source.NewLocal(adrs).ClosedAtoms()
}

// runGuard is the `lema-mcp guard` subcommand — the PreToolUse hook body. It reads
// the hook payload from stdin, loads the local decision store, and writes the
// permission decision to stdout. It always exits 0 and, on any error (bad JSON,
// missing store), emits nothing so a guard failure never blocks the agent —
// fail-open is correct for an advisory enforcement layer (ADR-0052).
func runGuard(args []string, refreshRuntime *hostedWriteRuntime) {
	// Fast-path: if guard is disabled, skip all file I/O and corpus loading.
	mode := os.Getenv(guardModeEnvVar)
	if mode == "" {
		mode = guardModeContext
	}
	if mode == guardModeOff {
		os.Exit(0)
	}

	capturePath := ".lema/decisions.jsonl"
	refreshCache := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--capture-file" && i+1 < len(args):
			capturePath = args[i+1]
		case args[i] == "--refresh-cache":
			refreshCache = true
		}
	}
	// In a linked git worktree the hook must enforce the repo's ONE store (the
	// main checkout's), not a frozen worktree copy (capture_path.go).
	capturePath = resolveCaptureFile(capturePath)

	// Refresh mode (F8, guard_cache.go): a SessionStart hook line fetches the
	// hosted closed set into the local cache and exits — the network stays off
	// the per-edit path entirely.
	if refreshCache {
		if refreshRuntime != nil {
			runGuardRefresh(capturePath, *refreshRuntime)
		}
		return
	}

	type readResult struct {
		data []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() { data, err := io.ReadAll(os.Stdin); ch <- readResult{data, err} }()
	var data []byte
	select {
	case r := <-ch:
		if r.err != nil {
			os.Exit(0)
		}
		data = r.data
	case <-time.After(3 * time.Second):
		os.Exit(0) // fail open on timeout
	}

	var in guardInput
	if err := json.Unmarshal(data, &in); err != nil {
		return
	}
	// Terminal mode: inside the lema terminal an attended human resolves the
	// interception, so the hook delegates to the terminal's serve --http sidecar
	// (which evaluates with this same matcher) instead of evaluating locally. Unlike
	// the unattended path below, a resolved :respect can bind a deny — that is the
	// human's call, not the matcher's (ADR-0052; see guard_terminal.go).
	if endpoint := os.Getenv(guardEndpointEnvVar); endpoint != "" {
		if out := guardViaTerminal(guardHTTPClient, endpoint, in); out != nil {
			if b, err := json.Marshal(out); err == nil {
				fmt.Println(string(b))
			}
		}
		return
	}
	query := guardQuery(in.ToolInput)
	// Tier-1 deterministic decision-checks (decisioncheck) run FIRST, before the
	// capture store is even loaded — a zero-FP rule that cites a specific closed
	// decision is a higher-confidence, more-actionable nudge than the lexical
	// never-reopen matcher, needs no capture store (so ADR enforcement holds even
	// in a repo with none), and is deliberately kept separate from verdict.Match
	// (d_045d82). The judged constraint tier is elsewhere behind its own eval;
	// this is only the deterministic lint. Design: docs/design/decision-quality-loop/.
	if out := decisionCheckGuard(in.ToolInput, mode); out != nil {
		guardLog(in, out, query, nil)
		if b, err := json.Marshal(out); err == nil {
			fmt.Println(string(b))
		}
		return
	}
	store, err := source.NewCaptureStore(capturePath)
	if err != nil {
		return
	}
	// Enforce off the forward-capture store, the repo's documented ADRs
	// (ADR-0053), AND the hosted closed-set cache (F8, guard_cache.go): a new
	// engineer's agent should be stopped by a decision the team recorded in an
	// ADR even if it was never captured live, and by a ruling recorded hosted
	// even if this machine never saw it land.
	closed := append(store.ClosedAtoms(), loadADRClosed(".")...)
	closed = append(closed, loadGuardCacheAtoms(capturePath)...)
	hits, suppressed := evaluateCitation(closed, query)
	out, atom := guardDecision(hits, mode)
	if out == nil {
		// Allow silently — but if the citation exemption is what suppressed a
		// fire, log it so the exemption's precision is measurable (ADR-0052).
		// guardLogWrite is a no-op when LEMA_GUARD_LOG is unset.
		if suppressed != nil {
			guardLogWrite(in, "citation-exempt", query, suppressed)
		}
		return
	}
	guardLog(in, out, query, atom)
	if b, err := json.Marshal(out); err == nil {
		fmt.Println(string(b))
	}
}

// guardLog appends one JSON line per fire to LEMA_GUARD_LOG when set — the
// substrate for measuring the guard's precision (false-positive rate) before v2
// promotes it to a hard block. It records the query, the match score, and the
// matched atom id/key so the gate can be calibrated, not guessed (ADR-0052).
// Silent when unset and never fatal: a logging failure must not change the
// permission decision.
func guardLog(in guardInput, out *guardOutput, query string, atom *source.Atom) {
	decision := out.HookSpecificOutput.PermissionDecision
	if decision == "" {
		decision = "context"
	}
	guardLogWrite(in, decision, query, atom)
}

// guardLogWrite is guardLog's core, also used for "citation-exempt" records —
// suppressed fires are logged with the atom they would have surfaced.
func guardLogWrite(in guardInput, decision, query string, atom *source.Atom) {
	path := os.Getenv("LEMA_GUARD_LOG")
	if path == "" {
		return
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	rec := map[string]any{
		"ts":       time.Now().UTC().Format(time.RFC3339),
		"tool":     in.ToolName,
		"mode":     os.Getenv(guardModeEnvVar),
		"decision": decision,
		"query":    redactSecrets(query),
	}
	if atom != nil {
		rec["score"] = atom.Score
		rec["atom"] = atom.Ref
		rec["match_key"] = atom.MatchKey
	}
	if b, err := json.Marshal(rec); err == nil {
		fmt.Fprintln(f, string(b))
	}
}
