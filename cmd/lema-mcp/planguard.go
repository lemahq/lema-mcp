package main

// plan-guard is the `lema-mcp plan-guard` subcommand — the same never-reopen
// judgment as the PreToolUse guard, but fed a STRUCTURED `terraform show -json`
// plan instead of a fuzzy code edit. Each resource change becomes a topic matched
// against the repo's closed-atom store (forward-captured decisions + ADRs) via the
// shared verdict.Match engine; a hit is emitted as an advisory markdown comment.
//
// It is the Phase-0 spike de-risking the Strata infra vertical (ADR-0090): a
// `terraform plan` is a cleaner decision-conflict input than a code diff because
// the changed resource identity and attribute KEYS are explicit. Honesty
// constraints, carried from the research and enforced here:
//
//   - Anchor to actions and resource identity, NEVER values. classifyAction reads
//     the always-known `actions` array; changedKeys reads attribute KEYS (stable),
//     not value precision — "(known after apply)" unknowns make values partial.
//   - Match on the recorded closed-decision text, never an invented why. emitReview
//     surfaces the stored ClosedNote only, mirroring evaluateGuard.
//   - Advisory-first. Parse error, missing store, no plan → emit nothing, exit 0.
//     An advisory layer must never block a deploy on its own bug. There is no
//     `deny` mode in v1 (LEMA_PLAN_GUARD_MODE=context only).

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
	"github.com/lemahq/lema-mcp/internal/verdict"
)

const (
	planGuardModeEnvVar  = "LEMA_PLAN_GUARD_MODE"
	planGuardModeContext = "context" // default and only v1 mode: advisory markdown, never blocks
	planGuardModeOff     = "off"     // kill switch
)

// tfPlan is the minimal subset of `terraform show -json` we read (stable
// format_version 1.x). Only resource_changes matters for the never-reopen check.
type tfPlan struct {
	ResourceChanges []resourceChange `json:"resource_changes"`
}

type resourceChange struct {
	Address string     `json:"address"` // "google_sql_database_instance.primary"
	Type    string     `json:"type"`    // "google_sql_database_instance"
	Name    string     `json:"name"`    // "primary"
	Change  planChange `json:"change"`
}

type planChange struct {
	// Actions is a closed set: no-op / create / read / update / delete; a
	// [delete,create] (or [create,delete]) pair is a replace. Always known.
	Actions      []string       `json:"actions"`
	Before       map[string]any `json:"before"`
	After        map[string]any `json:"after"`
	AfterUnknown map[string]any `json:"after_unknown"`
}

// sensitiveKeys are attribute names dropped from the query/log even though we only
// ever emit KEYS, never values — defense-in-depth so a credential-shaped key never
// reaches a PR comment or the calibration log.
var sensitiveKeys = map[string]bool{
	"password": true, "secret": true, "token": true,
	"private_key": true, "ssh_key": true, "certificate": true,
}

// classifyAction collapses the plan's actions array into one verb. It reads the
// ALWAYS-known actions field — never a value — so it is robust to unknowns.
func classifyAction(actions []string) string {
	if len(actions) == 0 {
		return "no-op"
	}
	if len(actions) >= 2 {
		// delete+create in either order is a replace.
		has := map[string]bool{}
		for _, a := range actions {
			has[a] = true
		}
		if has["delete"] && has["create"] {
			return "replace"
		}
	}
	return actions[0]
}

// changedKeys returns the attribute keys this change touches: keys whose value
// differs before→after, keys removed, and keys present in after_unknown (their
// value is "(known after apply)" — the KEY is still changing). Values are never
// returned; sensitive keys are dropped. Sorted for deterministic queries/tests.
func changedKeys(ch planChange) []string {
	keys := map[string]bool{}
	for k, av := range ch.After {
		bv, had := ch.Before[k]
		if !had || !reflect.DeepEqual(bv, av) {
			keys[k] = true
		}
	}
	for k := range ch.Before {
		if _, ok := ch.After[k]; !ok {
			keys[k] = true
		}
	}
	for k, v := range ch.AfterUnknown {
		if isUnknown(v) {
			keys[k] = true
		}
	}
	out := make([]string, 0, len(keys))
	for k := range keys {
		if sensitiveKeys[k] {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// isUnknown reports whether an after_unknown entry marks a key as computed at
// apply. Leaves are bools (true); nested objects/arrays mark a key unknown if any
// descendant is. We anchor on the key, so any truthy presence counts.
func isUnknown(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case map[string]any:
		for _, e := range t {
			if isUnknown(e) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if isUnknown(e) {
				return true
			}
		}
	}
	return false
}

// planQuery turns ONE resource change into the text the matcher scans. Pure and
// deterministic — code answers, the model does not. Signal = resource identity
// (type, name, address) plus the attribute keys that actually changed. no-op and
// read changes reopen nothing, so they are skipped.
func planQuery(rc resourceChange) (query, action string, skip bool) {
	action = classifyAction(rc.Change.Actions)
	if action == "no-op" || action == "read" {
		return "", action, true
	}
	parts := []string{rc.Type, rc.Name, rc.Address}
	parts = append(parts, changedKeys(rc.Change)...)
	return strings.Join(parts, " "), action, false
}

// planConflict is one resource change that reaches a settled decision.
type planConflict struct {
	Address string
	Action  string
	Atom    source.Atom
}

// scanPlan matches every actionable resource change against the closed-atom store
// and returns the top conflict per change. It is the TP/TN core: precision rests
// entirely on verdict.Match's distinctiveness weighting (a false never-reopen is
// an uninstall). No closed atoms → nil (nothing settled to reopen).
func scanPlan(plan tfPlan, closed []source.Atom) []planConflict {
	if len(closed) == 0 {
		return nil
	}
	var hits []planConflict
	for _, rc := range plan.ResourceChanges {
		q, action, skip := planQuery(rc)
		if skip {
			continue
		}
		matches := verdict.Match(closed, q, verdict.MatchThreshold)
		if len(matches) == 0 {
			continue
		}
		hits = append(hits, planConflict{Address: rc.Address, Action: action, Atom: matches[0]})
	}
	return hits
}

// emitReview renders the advisory markdown PR comment. It surfaces only the
// recorded ClosedNote (or, absent one, the option text) — never an invented why —
// and cites the decision's id so the override loop (record_decision --supersedes)
// is one click away. Empty string when clean: a silent guard says nothing.
func emitReview(hits []planConflict) string {
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## lema plan-guard — this plan reaches settled decisions\n\n")
	b.WriteString("Advisory only; this does not block your apply.\n\n")
	for _, h := range hits {
		reason := h.Atom.ClosedNote
		if reason == "" {
			reason = "this option was already ruled out: " + h.Atom.Text
		}
		fmt.Fprintf(&b, "- **`%s`** (%s) — %s", h.Address, h.Action, reason)
		if h.Atom.Ref != "" {
			fmt.Fprintf(&b, " _(decision `%s`)_", h.Atom.Ref)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nIf you are intentionally superseding one of these, run `record_decision` with `supersedes`; otherwise surface the prior decision before `apply`.\n")
	return b.String()
}

// planExitCode is the CI gate. v1 is advisory-only: it always returns 0 so the
// layer can never block a deploy on its own judgment (same contract as runGuard).
// A future `deny` mode would return nonzero here; deliberately not implemented yet.
func planExitCode(_ []planConflict, _ string) int {
	return 0
}

// loadPlanGuardClosed assembles the same closed-set as runGuard: forward-captured
// decisions plus the repo's documented ADRs. Both paths are fail-open — a missing
// capture file yields an empty store (not an error), and loadADRClosed returns nil
// on any discovery/parse failure.
func loadPlanGuardClosed(capturePath string) []source.Atom {
	var closed []source.Atom
	if store, err := source.NewCaptureStore(capturePath); err == nil {
		closed = append(closed, store.ClosedAtoms()...)
	}
	return append(closed, loadADRClosed(".")...)
}

// planGuardFlag returns the value of --name in args, or "" if absent.
func planGuardFlag(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

// readPlan reads the plan JSON from --plan <file> (or stdin when empty / "-").
// A read error is fail-open: ok=false, so the caller emits nothing and exits 0.
func readPlan(path string) (data []byte, ok bool) {
	var err error
	if path == "" || path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, false
	}
	return data, true
}

// planGuardRun is the testable core of the subcommand: it returns the exit code
// and writes any advisory review to out. Every failure mode is fail-open (exit 0,
// no output): disabled, unreadable/malformed plan, empty store.
func planGuardRun(args []string, out io.Writer) int {
	mode := os.Getenv(planGuardModeEnvVar)
	if mode == "" {
		mode = planGuardModeContext
	}
	if mode == planGuardModeOff {
		return 0
	}

	data, ok := readPlan(planGuardFlag(args, "--plan"))
	if !ok {
		return 0
	}
	var plan tfPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return 0 // fail-open on a malformed plan, like runGuard
	}

	capturePath := planGuardFlag(args, "--capture-file")
	if capturePath == "" {
		capturePath = ".lema/decisions.jsonl"
	}
	// Linked git worktree -> enforce the main checkout's store (capture_path.go).
	capturePath = resolveCaptureFile(capturePath)
	hits := scanPlan(plan, loadPlanGuardClosed(capturePath))

	if md := emitReview(hits); md != "" {
		fmt.Fprintln(out, md)
	}
	planGuardLog(hits, mode)
	return planExitCode(hits, mode)
}

// runPlanGuard is the `lema-mcp plan-guard` entry point. It wraps planGuardRun
// with the process exit so the core stays testable, and always exits 0 in v1.
func runPlanGuard(args []string) {
	os.Exit(planGuardRun(args, os.Stdout))
}

// planGuardLog appends one JSON line per fire to LEMA_PLAN_GUARD_LOG when set —
// the substrate for calibrating plan-guard's precision before any v2 hard block,
// mirroring guardLog. Silent when unset; never fatal.
func planGuardLog(hits []planConflict, mode string) {
	path := os.Getenv("LEMA_PLAN_GUARD_LOG")
	if path == "" || len(hits) == 0 {
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
	ts := time.Now().UTC().Format(time.RFC3339)
	for _, h := range hits {
		rec := map[string]any{
			"ts":        ts,
			"mode":      mode,
			"address":   h.Address,
			"action":    h.Action,
			"score":     h.Atom.Score,
			"atom":      h.Atom.Ref,
			"match_key": h.Atom.MatchKey,
		}
		if b, err := json.Marshal(rec); err == nil {
			fmt.Fprintln(f, string(b))
		}
	}
}
