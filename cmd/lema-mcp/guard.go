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
	"github.com/lemahq/lema-mcp/internal/source"
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
// confirmation on the very edit it is flagging — and never "deny" in v1.
// additionalContext is the non-blocking agent nudge; permissionDecisionReason is
// the "ask" reason (ADR-0052).
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
	q := strings.Join(parts, " ")
	if len(q) > guardMaxQuery {
		q = q[:guardMaxQuery]
	}
	return q
}

// tokenize splits s into lowercased alphanumeric tokens, breaking on
// non-alphanumeric runs AND camelCase boundaries, and dropping tokens shorter than
// 2 runes — so "kafka.NewProducer()" and "KafkaBrokers" both yield kafka/new/producer
// and kafka/brokers, and a killed option named inside an identifier still matches.
func tokenize(s string) []string {
	var toks []string
	var cur []rune
	var prev rune
	flush := func() {
		if len(cur) >= 2 {
			toks = append(toks, strings.ToLower(string(cur)))
		}
		cur = cur[:0]
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			prev = 0
			continue
		}
		// camelCase / digit boundary: a lower/digit run followed by an uppercase
		// letter starts a new token (NewProducer -> new, producer).
		if len(cur) > 0 && unicode.IsUpper(r) && (unicode.IsLower(prev) || unicode.IsDigit(prev)) {
			flush()
		}
		cur = append(cur, r)
		prev = r
	}
	flush()
	return toks
}

func tokenSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, t := range tokenize(s) {
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
	pieces := tokenize(key)
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

// guardMatch returns the CLOSED atoms whose killed option appears in the edit
// text, most-specific first, with Score set to the match specificity. It reads the
// option from atom.MatchKey (the option name / superseded choice), never the
// rationale prose (ADR-0052).
func guardMatch(closed []source.Atom, editText string) []source.Atom {
	edit := tokenSet(editText)
	if len(edit) == 0 {
		return nil
	}
	var out []source.Atom
	for _, a := range closed {
		if a.MatchKey == "" {
			continue
		}
		if ok, score := optionMatches(a.MatchKey, edit); ok {
			a.Score = score
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
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
	if mode == guardModeOff {
		return nil, nil
	}
	hits := guardMatch(closed, query)
	if len(hits) == 0 || hits[0].Score < guardMinScore {
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
func runGuard(args []string) {
	capturePath := ".lema/decisions.jsonl"
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--capture-file" {
			capturePath = args[i+1]
		}
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	var in guardInput
	if err := json.Unmarshal(data, &in); err != nil {
		return
	}
	store, err := source.NewCaptureStore(capturePath)
	if err != nil {
		return
	}
	mode := os.Getenv(guardModeEnvVar)
	if mode == "" {
		mode = guardModeContext
	}
	query := guardQuery(in.ToolInput)
	// Enforce off BOTH the forward-capture store and the repo's documented ADRs
	// (ADR-0053): a new engineer's agent should be stopped by a decision the team
	// recorded in an ADR even if it was never captured live.
	closed := append(store.ClosedAtoms(), loadADRClosed(".")...)
	out, atom := evaluateGuard(closed, query, mode)
	if out == nil {
		return // allow silently
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
	path := os.Getenv("LEMA_GUARD_LOG")
	if path == "" {
		return
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	decision := out.HookSpecificOutput.PermissionDecision
	if decision == "" {
		decision = "context"
	}
	rec := map[string]any{
		"ts":       time.Now().UTC().Format(time.RFC3339),
		"tool":     in.ToolName,
		"mode":     os.Getenv(guardModeEnvVar),
		"decision": decision,
		"query":    query,
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
