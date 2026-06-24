package main

import (
	"strings"
	"testing"
)

// The server `instructions` field is the proactive steering channel the host
// injects into the agent's context (ADR-0097/0124). The Directory criteria allow
// human-readable guidance here but BAN the imperative phrases banned in tool
// descriptions — an imperative steer delists lema. This guards BOTH server
// instruction strings (the public funnel's and the authed server's, which shipped
// blank) for that compliance, plus the load-bearing intents ADR-0124 locks: cede
// the API-how to a documentation tool (altitude — lema is the rationale oracle,
// not a worse Context7), and treat a silent record as "unknown," not "approved"
// (honesty). The test asserts WHY these strings matter, not just that they exist.
func TestServerInstructionsDirectoryCleanAndHonest(t *testing.T) {
	// The same banned set the tool-description test enforces (tools_test.go:72).
	banned := []string{"call this", "you must", "do not re-propose", "prefer this over", "instead of reading", "before you propose", "before you write"}
	cases := map[string]string{
		"public": publicServerInstructions,
		"authed": authedServerInstructions,
	}
	for name, instr := range cases {
		if strings.TrimSpace(instr) == "" {
			t.Errorf("%s server instructions are empty — the steering channel must not be blank (the dead-tool lever)", name)
			continue
		}
		low := strings.ToLower(instr)
		for _, b := range banned {
			if strings.Contains(low, b) {
				t.Errorf("%s server instructions contain Directory-banned imperative %q", name, b)
			}
		}
		if !strings.Contains(low, "documentation tool") {
			t.Errorf("%s server instructions must cede the how to a documentation tool (altitude rail, ADR-0124)", name)
		}
		if !strings.Contains(low, "unknown") {
			t.Errorf("%s server instructions must state a silent record means \"unknown,\" not approved (honesty rail)", name)
		}
	}
}
