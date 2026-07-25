// collector_checkpoint.go is the F4 checkpoint/handoff half of the open
// collector (pivot B2): on a run's boundary events (pre_compact, stop,
// session_end) the run's spooled envelopes distill — deterministically, no
// model call (ADR-0140 keeps this binary LLM-free) — into a per-project
// checkpoint; a later SessionStart from the same project injects it as
// additionalContext. Cross-run continuity is keyed on the project directory
// (evidence.cwd), mirroring the association ladder's rung-4 worktree
// continuity server-side — never a tab id (P8's paused mechanism), and a
// fresh session's new session_id cannot self-link.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	checkpointMaxPrompts = 3
	checkpointMaxFiles   = 8
)

// collectorSyncerForCheckpoint is a test seam over the CWD-bound constructor.
// SessionStart always builds its target context from checkpoint evidence, never
// the hook process's ambient directory.
var collectorSyncerForCheckpoint = newCollectorSyncerForCWD

// collectorCheckpoint is the distilled handoff state for one project dir.
// RunID records which run produced it, so the injected block is attributable.
// Harness rides along because the hosted run identity is keyed on
// (harness, external_run_id) — a resolver using a different harness value
// would mint a second identity for the same session.
type collectorCheckpoint struct {
	CWD           string   `json:"cwd"`
	RunID         string   `json:"run_id"`
	Harness       string   `json:"harness,omitempty"`
	UpdatedAt     string   `json:"updated_at"`
	Summary       string   `json:"summary"`
	RecentPrompts []string `json:"recent_prompts,omitempty"`
	FilesTouched  []string `json:"files_touched,omitempty"`
	EventCount    int      `json:"event_count"`
}

// checkpointKey collapses a project path into a stable filename key. The
// sanitized name alone collides across slash-vs-dash siblings (apps/web vs
// apps-web), so a short content hash of the raw path disambiguates; the
// readable prefix stays for operator legibility.
func checkpointKey(cwd string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(cwd))
	name := sanitizeTabID(strings.TrimPrefix(cwd, string(filepath.Separator)))
	return fmt.Sprintf("%s-%08x", name, h.Sum32())
}

func collectorCheckpointPath(dir, cwd string) string {
	return filepath.Join(dir, "checkpoints", checkpointKey(cwd)+".json")
}

// distillEnvelopes reduces one run's envelopes to a checkpoint. Deterministic
// selection only: last prompts, files touched, counts — judgment stays human.
func distillEnvelopes(envs []collectorEnvelope, cwd string) collectorCheckpoint {
	cp := collectorCheckpoint{
		CWD:        cwd,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
		EventCount: len(envs),
	}
	seen := map[string]bool{}
	for _, ev := range envs {
		if cp.RunID == "" {
			cp.RunID = ev.RunID
		}
		if cp.Harness == "" {
			cp.Harness = ev.Evidence["harness"]
		}
		if fp := ev.Payload["file_path"]; fp != "" && !seen[fp] {
			seen[fp] = true
			cp.FilesTouched = append(cp.FilesTouched, fp)
		}
	}
	if len(cp.FilesTouched) > checkpointMaxFiles {
		cp.FilesTouched = cp.FilesTouched[len(cp.FilesTouched)-checkpointMaxFiles:]
	}
	for i := len(envs) - 1; i >= 0 && len(cp.RecentPrompts) < checkpointMaxPrompts; i-- {
		if p := envs[i].Payload["prompt"]; p != "" {
			cp.RecentPrompts = append([]string{p}, cp.RecentPrompts...)
		}
	}
	var parts []string
	if n := len(cp.RecentPrompts); n > 0 {
		last := cp.RecentPrompts[n-1]
		if len([]rune(last)) > 120 {
			last = string([]rune(last)[:120]) + "…"
		}
		parts = append(parts, "last prompt: "+last)
	}
	if len(cp.FilesTouched) > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s) touched", len(cp.FilesTouched)))
	}
	if cp.EventCount > 0 {
		parts = append(parts, fmt.Sprintf("%d events collected", cp.EventCount))
	}
	if len(parts) == 0 {
		cp.Summary = "run started — no activity collected yet"
	} else {
		cp.Summary = strings.Join(parts, "; ")
	}
	return cp
}

func writeCollectorCheckpoint(dir string, cp collectorCheckpoint) error {
	path := collectorCheckpointPath(dir, cp.CWD)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// readCollectorCheckpoint returns the project's checkpoint if one exists and
// is younger than the collector TTL — an expired handoff is stale context and
// injecting it would misdirect the next session.
func readCollectorCheckpoint(dir, cwd string, now time.Time) (collectorCheckpoint, bool) {
	b, err := os.ReadFile(collectorCheckpointPath(dir, cwd))
	if err != nil {
		return collectorCheckpoint{}, false
	}
	var cp collectorCheckpoint
	if json.Unmarshal(b, &cp) != nil || cp.Summary == "" {
		return collectorCheckpoint{}, false
	}
	// Never serve another project's state: the stored cwd must match the
	// asking cwd exactly, whatever the filename said.
	if cp.CWD != cwd {
		return collectorCheckpoint{}, false
	}
	if at, err := time.Parse(time.RFC3339, cp.UpdatedAt); err != nil || now.Sub(at) > collectorTTL {
		return collectorCheckpoint{}, false
	}
	return cp, true
}

// formatCheckpointBlock renders the injected continuity block. Honest
// provenance: it names the producing run and when it last updated.
func formatCheckpointBlock(cp collectorCheckpoint) string {
	var b strings.Builder
	fmt.Fprintf(&b, "lema handoff checkpoint for this project (run %s, updated %s):\n%s\n", cp.RunID, cp.UpdatedAt, cp.Summary)
	if len(cp.RecentPrompts) > 0 {
		b.WriteString("\nRecent prompts:\n")
		for i, p := range cp.RecentPrompts {
			fmt.Fprintf(&b, "%d. %s\n", i+1, p)
		}
	}
	if len(cp.FilesTouched) > 0 {
		b.WriteString("\nFiles touched:\n")
		for _, f := range cp.FilesTouched {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	b.WriteString("\nTreat this as continuity from a prior session in this project — do not re-litigate settled work unless the user asks.")
	return b.String()
}

// checkpointStateBrief is the SessionStart hosted relay read. The F4
// checkpoint remains the fail-open source of continuity, while the hosted
// projection becomes the preferred payload when it is complete and available.
func checkpointStateBrief(cp collectorCheckpoint) (string, bool) {
	s := collectorSyncerForCheckpoint(cp.CWD)
	if s == nil || s.runtime == nil {
		return "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), collectorSyncTimeout)
	defer cancel()

	input := s.runtime.targetInput
	if input.CWD != cp.CWD {
		// A local association belongs only to the CWD that loaded it. Do not
		// carry ambient process evidence into a checkpoint-owned relay read.
		input.CWD = cp.CWD
		input.LocalAssociation = nil
		input.PersistedAssociation = false
	}
	type resolvedBrief struct {
		output stateBriefOutput
		runID  string
	}
	result, err := withResolvedTarget(ctx, s.runtime.targets, input, func(ctx context.Context, receipt targetContext) (resolvedBrief, error) {
		harness := strings.TrimSpace(cp.Harness)
		if harness == "" {
			harness = "claude-code"
		}
		run, err := s.ensureRunInWorkspace(ctx, receipt.ProjectWorkspaceID, harness, cp.RunID, cp.CWD)
		if err != nil {
			return resolvedBrief{}, err
		}
		briefSyncer := *s
		briefSyncer.workspaceID = receipt.ProjectWorkspaceID
		return resolvedBrief{
			output: stateBriefForReceipt(ctx, &briefSyncer, receipt, run.ID, "SessionStart"),
			runID:  run.ID,
		}, nil
	})
	if err != nil {
		return "", false
	}
	return formatStateBriefBlock(result.output, result.runID)
}

type injectedStateBriefSection struct {
	Name  string `json:"name"`
	Lines []struct {
		Text string `json:"text"`
		Cite string `json:"cite"`
	} `json:"lines"`
}

// formatStateBriefBlock emits the server's deterministic projection compactly:
// scope, every section and cited line, and every declared silence. It does not
// invent a next action or blockers when those source classes are silent.
func formatStateBriefBlock(out stateBriefOutput, hostedRunID string) (string, bool) {
	if strings.TrimSpace(out.Scope) == "" {
		return "", false
	}
	var sections []injectedStateBriefSection
	if out.Sections != nil {
		raw, err := json.Marshal(out.Sections)
		if err != nil || json.Unmarshal(raw, &sections) != nil {
			return "", false
		}
	}
	var silences []string
	if out.Silences != nil {
		raw, err := json.Marshal(out.Silences)
		if err != nil || json.Unmarshal(raw, &silences) != nil {
			return "", false
		}
	}

	if strings.TrimSpace(hostedRunID) == "" {
		return "", false
	}

	webURL := strings.TrimRight(strings.TrimSpace(os.Getenv(settleWebURLEnv)), "/")
	if webURL == "" {
		webURL = defaultSettleWebURL
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Lema State Brief (hosted run %s; %s/briefing?run=%s)\n", hostedRunID, webURL, url.QueryEscape(hostedRunID))
	fmt.Fprintf(&b, "Scope: %s\n", out.Scope)
	b.WriteString("\nSections:\n")
	if len(sections) == 0 {
		b.WriteString("- none\n")
	}
	for _, section := range sections {
		if strings.TrimSpace(section.Name) == "" {
			return "", false
		}
		fmt.Fprintf(&b, "%s:\n", section.Name)
		for _, line := range section.Lines {
			if strings.TrimSpace(line.Text) == "" {
				return "", false
			}
			if strings.TrimSpace(line.Cite) == "" {
				fmt.Fprintf(&b, "- %s\n", line.Text)
			} else {
				fmt.Fprintf(&b, "- %s [%s]\n", line.Text, line.Cite)
			}
		}
	}
	b.WriteString("\nSilences:\n")
	if len(silences) == 0 {
		b.WriteString("- none\n")
	}
	for _, silence := range silences {
		if strings.TrimSpace(silence) == "" {
			return "", false
		}
		fmt.Fprintf(&b, "- %s\n", silence)
	}
	return b.String(), true
}

// checkpointOnBoundary distills and writes the checkpoint when the incoming
// envelope marks a run boundary; injectOnStart emits the prior checkpoint on
// a session start. Both are called from runCollect and stay fail-open.
func checkpointOnBoundary(dir string, ev collectorEnvelope) {
	switch ev.Kind {
	case "pre_compact", "stop", "session_end":
	default:
		return
	}
	cwd := ev.Evidence["cwd"]
	if cwd == "" {
		return
	}
	envs, err := readRunEnvelopes(dir, ev.RunID)
	if err != nil {
		return
	}
	// A run resumed from a different directory may span cwds; this project's
	// checkpoint distills only the envelopes produced here.
	var here []collectorEnvelope
	for _, e := range envs {
		if e.Evidence["cwd"] == cwd {
			here = append(here, e)
		}
	}
	if len(here) == 0 {
		return
	}
	_ = writeCollectorCheckpoint(dir, distillEnvelopes(here, cwd))
}

func injectOnStart(dir string, ev collectorEnvelope) {
	if ev.Kind != "session_start" {
		return
	}
	cwd := ev.Evidence["cwd"]
	if cwd == "" {
		return
	}
	if cp, ok := readCollectorCheckpoint(dir, cwd, time.Now()); ok {
		if brief, ok := checkpointStateBrief(cp); ok {
			emitAdditionalContext("SessionStart", brief)
			return
		}
		emitAdditionalContext("SessionStart", formatCheckpointBlock(cp))
	}
}
