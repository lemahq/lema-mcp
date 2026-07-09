// Package decisioncheck is tier-1 of the decision-quality loop: deterministic
// enforcement of recorded decisions on a code change. Every rule is a pure
// grep/AST predicate citing a CLOSED decision atom — NO model calls, NO
// judgment. A rule fires only when it is certain, because a false accusation
// ("you violated a recorded decision") is an uninstall (design-lock: the
// zero-FP contract). The judged, precision-gated tier (constraint atoms via
// retrieve-then-judge) is tier-2 and lives elsewhere behind its own eval.
//
// This is the enforcement engine; both homes — the edit-time PreToolUse guard
// and the PR-time verify check — iterate the same Rules() slice over a Change.
// Design: docs/design/decision-quality-loop/.
package decisioncheck

import "regexp"

// Change is the unit a rule inspects: the new content being written plus the
// file it targets. Deliberately minimal — tier-1 rules are pure predicates over
// the new text + path, which is exactly what the guard has in scope (Edit
// new_string / Write content) and what a diff hunk's added lines provide.
type Change struct {
	Path    string // repo-relative path being edited or created
	NewText string // the new content only (never the removed text)
}

// Finding is a deterministic rule hit: the rule that fired, the decision it
// cites, and an actionable message (what's wrong + the remedy).
type Finding struct {
	Rule    string `json:"rule"`
	Cite    string `json:"cite"`
	Message string `json:"message"`
}

// Rule is a named deterministic check citing a closed decision. Check returns
// zero or more findings for a change and MUST NOT return a finding it isn't
// certain of — the zero-FP contract is the whole license for tier-1 to run
// without a precision eval (design-lock: "deterministic lint is NOT A4").
type Rule struct {
	Name  string
	Cite  string
	Check func(Change) []Finding
}

// Rules is the registry — a thin slice (design-lock M5: data enough to add a
// rule without a rewrite, no predicate framework). Both homes iterate it.
func Rules() []Rule {
	return []Rule{adr0111AuthedEnvGate()}
}

// Check runs every rule over a change and returns all findings, in registry
// order. A change that trips nothing returns nil (silence = no violation of the
// encoded rules, never "decision-safe" — the caller owns that copy).
func Check(c Change) []Finding {
	var out []Finding
	for _, r := range Rules() {
		out = append(out, r.Check(c)...)
	}
	return out
}

// --- ADR-0111: runtime-toggleable authed surfaces gate on WorkOS Feature
// Flags, not ad-hoc env checks. The deterministic signal for the drift is a
// new `process.env.LEMA_*_ENABLED` launch gate in the web launch-mode module —
// the exact preserved fixture (decision 05c033f8). Scoped to launch-mode.ts:
// an authed launch gate is that file's job, so the pattern there is
// unambiguously the drift, while an api/infra LEMA_*_ENABLED read can be a
// legitimate kill-switch (ADR-0111 carves those out to env/Terraform) and must
// not be accused.

var authedEnvGateRe = regexp.MustCompile(`process\.env\.LEMA_[A-Z0-9_]+_ENABLED`)

func adr0111AuthedEnvGate() Rule {
	return Rule{
		Name: "adr-0111-authed-env-gate",
		Cite: "ADR-0111",
		Check: func(c Change) []Finding {
			if !isWebLaunchMode(c.Path) {
				return nil
			}
			if !authedEnvGateRe.MatchString(c.NewText) {
				return nil
			}
			return []Finding{{
				Rule:    "adr-0111-authed-env-gate",
				Cite:    "ADR-0111",
				Message: "This reintroduces an env-var launch gate (process.env.LEMA_*_ENABLED) for an authed surface. ADR-0111 requires a WorkOS Feature Flag for runtime-toggleable authed surfaces — add a slug to feature-flags.ts and gate on isFlagEnabled(), not an env check.",
			}}
		},
	}
}

// isWebLaunchMode matches the web launch-mode module regardless of the checkout
// root (the guard sees an absolute path; the verify diff sees a repo-relative
// one).
func isWebLaunchMode(path string) bool {
	return regexpLaunchMode.MatchString(path)
}

var regexpLaunchMode = regexp.MustCompile(`(^|/)apps/web/lib/launch-mode\.ts$`)
