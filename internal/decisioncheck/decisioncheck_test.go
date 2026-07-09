package decisioncheck

import (
	"strings"
	"testing"
)

// Every test encodes the wedge: a change that reintroduces a pattern the team
// already ruled against is flagged, citing the decision — and, because a false
// accusation is an uninstall (design-lock), the zero-FP contract is asserted
// harder than the fire cases. Most tests here assert NOT firing.

// The preserved fixture (decision 05c033f8): the record-conflicts panel was
// gated on an env var, violating ADR-0111. This is the exact drift the Phase-0
// gate must catch.
const fixtureNewText = `export function recordConflictsEnabled(): boolean {
  return process.env.LEMA_RECORD_CONFLICTS_ENABLED === 'true';
}`

func hasFinding(fs []Finding, cite string) bool {
	for _, f := range fs {
		if f.Cite == cite {
			return true
		}
	}
	return false
}

func TestADR0111_FiresOnTheFixture(t *testing.T) {
	fs := Check(Change{Path: "apps/web/lib/launch-mode.ts", NewText: fixtureNewText})
	if !hasFinding(fs, "ADR-0111") {
		t.Fatalf("must flag the preserved ADR-0111 fixture, got %+v", fs)
	}
	// The finding must name the decision and say what to do instead — a bare
	// "violation" with no remedy is not actionable.
	var f Finding
	for _, x := range fs {
		if x.Cite == "ADR-0111" {
			f = x
		}
	}
	if !strings.Contains(strings.ToLower(f.Message), "workos") {
		t.Errorf("message must point at the WorkOS-flag remedy, got %q", f.Message)
	}
}

// Zero-FP: a launch-mode edit that does NOT introduce the env-gate pattern is
// silent. The file is in scope but the pattern isn't present.
func TestADR0111_SilentOnCleanLaunchModeEdit(t *testing.T) {
	clean := `export function homePath(): string {
  return briefingEnabled() ? '/briefing' : '/ask';
}`
	if fs := Check(Change{Path: "apps/web/lib/launch-mode.ts", NewText: clean}); len(fs) != 0 {
		t.Fatalf("clean launch-mode edit must not fire, got %+v", fs)
	}
}

// Zero-FP: the WorkOS-flag path (the CORRECT pattern ADR-0111 mandates) must
// never be flagged — flagging the fix is the worst false positive.
func TestADR0111_SilentOnTheCorrectWorkOSPattern(t *testing.T) {
	correct := `export async function recordConflictsEnabled(ctx?: FlagContext): Promise<boolean> {
  return isFlagEnabled(FLAGS.RECORD_CONFLICTS, ctx);
}`
	if fs := Check(Change{Path: "apps/web/lib/feature-flags.ts", NewText: correct}); len(fs) != 0 {
		t.Fatalf("the WorkOS-flag remedy must never be flagged, got %+v", fs)
	}
}

// Scoping: the env-gate pattern outside the web launch-mode module is out of
// this rule's scope — an infra/api LEMA_*_ENABLED read can be a legitimate
// kill-switch (ADR-0111 carves those out to env/Terraform). Firing there would
// be a false accusation.
func TestADR0111_ScopedToLaunchMode(t *testing.T) {
	apiKillSwitch := `if os.Getenv("LEMA_AGENT_DEMO_ENABLED") == "true" {`
	if fs := Check(Change{Path: "apps/api/internal/api/agent_public.go", NewText: apiKillSwitch}); len(fs) != 0 {
		t.Fatalf("an api-tier LEMA_*_ENABLED read is a kill-switch, not an authed launch gate — must not fire, got %+v", fs)
	}
}

// A non-_ENABLED env read in launch-mode is not a launch gate (e.g. a URL) —
// the pattern is specifically the *_ENABLED launch-gate suffix.
func TestADR0111_OnlyEnabledSuffix(t *testing.T) {
	notAGate := `const base = process.env.LEMA_WEB_URL ?? 'http://localhost:3000';`
	if fs := Check(Change{Path: "apps/web/lib/launch-mode.ts", NewText: notAGate}); len(fs) != 0 {
		t.Fatalf("a non-_ENABLED env read is not a launch gate, got %+v", fs)
	}
}

// Registry sanity: every rule cites a decision (a finding with no citation is
// not actionable and violates the "cite the decision" wedge).
func TestEveryRuleCitesADecision(t *testing.T) {
	for _, r := range Rules() {
		if strings.TrimSpace(r.Cite) == "" {
			t.Errorf("rule %q has no citation", r.Name)
		}
		if strings.TrimSpace(r.Name) == "" {
			t.Errorf("rule with cite %q has no name", r.Cite)
		}
	}
}
