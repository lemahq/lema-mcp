package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// F4's contract: run boundaries distill deterministically into a per-project
// checkpoint keyed on cwd (rung-4-style continuity — never a tab id), and a
// later SessionStart from the same project injects it with honest provenance
// (producing run + age); an expired checkpoint is never injected.

func mkEnv(runID, kind string, payload map[string]string) collectorEnvelope {
	return collectorEnvelope{
		RunID:   runID,
		TS:      time.Now().UTC().Format(time.RFC3339),
		Kind:    kind,
		Payload: payload,
		Evidence: map[string]string{
			"harness": "claude-code",
			"cwd":     "/repo/proj",
		},
	}
}

func TestDistillEnvelopesSelectsDeterministically(t *testing.T) {
	envs := []collectorEnvelope{
		mkEnv("r1", "session_start", nil),
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "first ask"}),
		mkEnv("r1", "tool_use", map[string]string{"tool_name": "Edit", "file_path": "a.go"}),
		mkEnv("r1", "tool_use", map[string]string{"tool_name": "Edit", "file_path": "a.go"}),
		mkEnv("r1", "tool_use", map[string]string{"tool_name": "Write", "file_path": "b.go"}),
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "second ask"}),
	}
	cp := distillEnvelopes(envs, "/repo/proj", collectorCheckpoint{})
	if cp.RunID != "r1" || cp.CWD != "/repo/proj" || cp.EventCount != 6 {
		t.Fatalf("cp = %#v", cp)
	}
	if len(cp.FilesTouched) != 2 || cp.FilesTouched[0] != "a.go" || cp.FilesTouched[1] != "b.go" {
		t.Fatalf("files must dedupe in order, got %v", cp.FilesTouched)
	}
	if len(cp.RecentPrompts) != 2 || cp.RecentPrompts[1] != "second ask" {
		t.Fatalf("prompts = %v", cp.RecentPrompts)
	}
	if !strings.Contains(cp.Summary, "second ask") || !strings.Contains(cp.Summary, "2 file(s)") {
		t.Fatalf("summary = %q", cp.Summary)
	}
}

func TestCheckpointRoundTripAndExpiry(t *testing.T) {
	dir := t.TempDir()
	cp := distillEnvelopes([]collectorEnvelope{mkEnv("r1", "user_prompt", map[string]string{"prompt": "x"})}, "/repo/proj", collectorCheckpoint{})
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now()); !ok {
		t.Fatal("fresh checkpoint must read back")
	}
	if _, ok := readCollectorCheckpoint(dir, "/other", time.Now()); ok {
		t.Fatal("a different project must not see this checkpoint")
	}
	if _, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now().Add(collectorTTL+time.Hour)); ok {
		t.Fatal("an expired checkpoint must never inject")
	}
}

func TestCheckpointOnBoundaryWritesOnlyOnBoundaries(t *testing.T) {
	dir := t.TempDir()
	for _, ev := range []collectorEnvelope{
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "work work"}),
		mkEnv("r1", "tool_use", map[string]string{"file_path": "a.go"}),
	} {
		if err := appendEnvelope(dir, ev); err != nil {
			t.Fatal(err)
		}
		checkpointOnBoundary(dir, ev) // non-boundary kinds: no checkpoint
	}
	if _, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now()); ok {
		t.Fatal("non-boundary events must not checkpoint")
	}
	// runCollect distills BEFORE appending the boundary marker (run_event.go's
	// order): the stop itself is not activity and stays out of its checkpoint.
	stop := mkEnv("r1", "stop", nil)
	checkpointOnBoundary(dir, stop)
	if err := appendEnvelope(dir, stop); err != nil {
		t.Fatal(err)
	}
	cp, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now())
	if !ok {
		t.Fatal("stop must write the checkpoint")
	}
	if cp.RunID != "r1" || cp.EventCount != 2 {
		t.Fatalf("cp = %#v (boundary marker must not count as activity)", cp)
	}
}

func TestCheckpointKeyDisambiguatesSlashDashSiblings(t *testing.T) {
	if checkpointKey("/repo/apps/web") == checkpointKey("/repo/apps-web") {
		t.Fatal("slash/dash sibling projects must not share a checkpoint file")
	}
}

func TestCheckpointFiltersForeignCwdEnvelopes(t *testing.T) {
	dir := t.TempDir()
	foreign := mkEnv("r1", "tool_use", map[string]string{"file_path": "secret.go"})
	foreign.Evidence["cwd"] = "/elsewhere"
	for _, ev := range []collectorEnvelope{
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "here"}),
		foreign,
	} {
		if err := appendEnvelope(dir, ev); err != nil {
			t.Fatal(err)
		}
	}
	checkpointOnBoundary(dir, mkEnv("r1", "stop", nil))
	cp, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now())
	if !ok {
		t.Fatal("checkpoint expected")
	}
	if cp.EventCount != 1 || len(cp.FilesTouched) != 0 {
		t.Fatalf("a resumed run's other-project envelopes must not leak in: %#v", cp)
	}
}

func TestReadCheckpointRefusesForeignCWD(t *testing.T) {
	dir := t.TempDir()
	cp := distillEnvelopes([]collectorEnvelope{mkEnv("r1", "user_prompt", map[string]string{"prompt": "x"})}, "/repo/proj", collectorCheckpoint{})
	cp.CWD = "/somewhere/else" // simulate a mis-filed or corrupted checkpoint
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCollectorCheckpoint(dir, "/somewhere/else", time.Now()); !ok {
		t.Fatal("sanity: the checkpoint reads back under its own cwd")
	}
	if _, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now()); ok {
		t.Fatal("a checkpoint whose stored cwd differs must never be served")
	}
}

// captureStdout lives in settle_test.go.

func TestInjectOnStartEmitsAdditionalContext(t *testing.T) {
	dir := t.TempDir()
	cp := distillEnvelopes([]collectorEnvelope{
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "ship F4"}),
		mkEnv("r1", "tool_use", map[string]string{"file_path": "collector.go"}),
	}, "/repo/proj", collectorCheckpoint{})
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		injectOnStart(dir, mkEnv("r2-new-session", "session_start", nil))
	})
	var hook guardOutput
	if err := json.Unmarshal([]byte(out), &hook); err != nil {
		t.Fatalf("stdout must be one hook JSON object, got %q: %v", out, err)
	}
	ctx := hook.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "ship F4") || !strings.Contains(ctx, "collector.go") {
		t.Fatalf("injected context missing distilled state: %q", ctx)
	}
	if !strings.Contains(ctx, "run r1") {
		t.Fatalf("injection must attribute the producing run: %q", ctx)
	}
}

func TestInjectOnStartPrefersHostedStateBrief(t *testing.T) {
	dir := t.TempDir()
	cp := distillEnvelopes([]collectorEnvelope{
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "ship F4"}),
	}, "/repo/proj", collectorCheckpoint{})
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}

	srv, capture := newBriefTestServer(t, "claude-code", 200)
	defer srv.Close()
	provider := &fakeTargetProvider{result: resolutionResult{
		Status: resolutionResolved, Context: stateBriefRoutingContext(),
	}}
	syncer := &collectorSyncer{
		apiURL: srv.URL, token: "lema_live_routing_secret", client: srv.Client(),
		runtime: &hostedWriteRuntime{
			apiURL: srv.URL, token: "lema_live_routing_secret", client: srv.Client(),
			targets: provider,
		},
	}
	original := collectorSyncerForCheckpoint
	collectorSyncerForCheckpoint = func(cwd string) *collectorSyncer {
		if cwd != cp.CWD {
			t.Errorf("syncer cwd = %q, want checkpoint cwd %q", cwd, cp.CWD)
		}
		return syncer
	}
	t.Cleanup(func() { collectorSyncerForCheckpoint = original })

	out := captureStdout(t, func() {
		injectOnStart(dir, mkEnv("r2-new-session", "session_start", nil))
	})
	var hook guardOutput
	if err := json.Unmarshal([]byte(out), &hook); err != nil {
		t.Fatalf("stdout must be one hook JSON object, got %q: %v", out, err)
	}
	ctx := hook.HookSpecificOutput.AdditionalContext
	for _, want := range []string{
		"Lema State Brief", "Scope: work unit wu-1", "objective:",
		"ship it [work_unit:wu-1]", "Silences:",
		"test status — not captured in v1",
		"https://lema.sh/briefing?run=" + briefRunID,
	} {
		if !strings.Contains(ctx, want) {
			t.Errorf("hosted brief missing %q:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "lema handoff checkpoint for this project") {
		t.Fatalf("hosted success must replace the local checkpoint identity:\n%s", ctx)
	}
	if capture.runCreates != 1 || len(capture.briefRuns) != 1 || capture.briefRuns[0] != briefRunID {
		t.Fatalf("hosted run resolution = creates %d, brief runs %v", capture.runCreates, capture.briefRuns)
	}
}

func TestInjectOnStartFallsBackWhenHostedBriefUnavailable(t *testing.T) {
	dir := t.TempDir()
	cp := distillEnvelopes([]collectorEnvelope{
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "ship F4"}),
	}, "/repo/proj", collectorCheckpoint{})
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}

	srv, _ := newBriefTestServer(t, "claude-code", 404)
	defer srv.Close()
	syncer := &collectorSyncer{
		apiURL: srv.URL, token: "lema_live_routing_secret", client: srv.Client(),
		runtime: &hostedWriteRuntime{
			apiURL: srv.URL, token: "lema_live_routing_secret", client: srv.Client(),
			targets: &fakeTargetProvider{result: resolutionResult{
				Status: resolutionResolved, Context: stateBriefRoutingContext(),
			}},
		},
	}
	original := collectorSyncerForCheckpoint
	collectorSyncerForCheckpoint = func(string) *collectorSyncer { return syncer }
	t.Cleanup(func() { collectorSyncerForCheckpoint = original })

	out := captureStdout(t, func() {
		injectOnStart(dir, mkEnv("r2-new-session", "session_start", nil))
	})
	var hook guardOutput
	if err := json.Unmarshal([]byte(out), &hook); err != nil {
		t.Fatalf("stdout must be one hook JSON object, got %q: %v", out, err)
	}
	ctx := hook.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "lema handoff checkpoint for this project") ||
		!strings.Contains(ctx, "ship F4") {
		t.Fatalf("hosted failure must preserve local continuity:\n%s", ctx)
	}
	if strings.Contains(ctx, "Lema State Brief") {
		t.Fatalf("unavailable hosted brief must not emit a partial brief:\n%s", ctx)
	}
}

func TestInjectOnStartSilentWhenNoCheckpoint(t *testing.T) {
	dir := t.TempDir()
	out := captureStdout(t, func() {
		injectOnStart(dir, mkEnv("r9", "session_start", nil))
	})
	if strings.TrimSpace(out) != "" {
		t.Fatalf("no checkpoint → no stdout, got %q", out)
	}
}

// The self-predecessor bug (state_brief_tool.go's resolvePriorRun): a run
// boundary write (checkpointOnBoundary) overwrites RunID with the CURRENT
// run's own id, so a later read within that same session was resolving
// itself as "the prior run". PreviousRunID exists to survive that
// overwrite — these three tests pin the write-side half of the fix;
// TestResolvePriorRunRefusesSelf (state_brief_tool_test.go) pins the read
// side.

func TestDistillEnvelopesCapturesTransitionToNewRun(t *testing.T) {
	previous := collectorCheckpoint{CWD: "/repo/proj", RunID: "r1", PreviousRunID: "r0"}
	cp := distillEnvelopes([]collectorEnvelope{
		mkEnv("r2", "user_prompt", map[string]string{"prompt": "new session"}),
	}, "/repo/proj", previous)
	if cp.RunID != "r2" {
		t.Fatalf("RunID = %q, want the new run r2", cp.RunID)
	}
	if cp.PreviousRunID != "r1" {
		t.Fatalf("PreviousRunID = %q, want r1 (the run r2 took over from) — not r0, r2's own grandparent", cp.PreviousRunID)
	}
}

func TestDistillEnvelopesPreservesPreviousRunIDAcrossSameRunRewrite(t *testing.T) {
	previous := collectorCheckpoint{CWD: "/repo/proj", RunID: "r2", PreviousRunID: "r1"}
	cp := distillEnvelopes([]collectorEnvelope{
		mkEnv("r2", "user_prompt", map[string]string{"prompt": "turn 3, same run"}),
	}, "/repo/proj", previous)
	if cp.RunID != "r2" || cp.PreviousRunID != "r1" {
		t.Fatalf("cp = %#v, want RunID=r2 PreviousRunID=r1 preserved (never re-derived from r2 itself)", cp)
	}
}

func TestDistillEnvelopesNoPreviousRunIDWithoutAKnownPredecessor(t *testing.T) {
	for name, previous := range map[string]collectorCheckpoint{
		"genesis, no prior checkpoint at all": {},
		"prior checkpoint belongs to a different cwd": {CWD: "/other", RunID: "r0"},
	} {
		t.Run(name, func(t *testing.T) {
			cp := distillEnvelopes([]collectorEnvelope{
				mkEnv("r1", "user_prompt", map[string]string{"prompt": "first ever"}),
			}, "/repo/proj", previous)
			if cp.PreviousRunID != "" {
				t.Fatalf("PreviousRunID = %q, want empty (honest: no predecessor known)", cp.PreviousRunID)
			}
		})
	}
}

func TestInjectOnStartStampsPreviousRunID(t *testing.T) {
	dir := t.TempDir()
	cp := distillEnvelopes([]collectorEnvelope{
		mkEnv("r1", "user_prompt", map[string]string{"prompt": "ship F4"}),
	}, "/repo/proj", collectorCheckpoint{})
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	captureStdout(t, func() {
		injectOnStart(dir, mkEnv("r2-new-session", "session_start", nil))
	})
	stamped, ok := readCollectorCheckpoint(dir, "/repo/proj", time.Now())
	if !ok {
		t.Fatal("checkpoint must still read back after injectOnStart")
	}
	if stamped.PreviousRunID != "r1" {
		t.Fatalf("PreviousRunID = %q, want r1 frozen before this new session can overwrite RunID", stamped.PreviousRunID)
	}
}
