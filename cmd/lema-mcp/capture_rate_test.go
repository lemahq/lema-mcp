package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The capture-rate gauge is the heartbeat instrument (roadmap Now #1; the
// rule-4 "instrument before the thing it instruments" gate): an empty graph
// saves nothing, and this is how we know whether capture is happening at all.
// The metric contract these tests pin:
//
//	numerator   = GENUINE record_decision calls — structurally validated by
//	              extractDecision (substring mentions are prose noise), deduped
//	              per session by the same ref/title key the Sessions surface uses
//	denominator = decision-shaped moments — manifest-file edits, classified by
//	              the SAME predicate the capture nudge fires on, so the gauge
//	              measures exactly what the nudge nags about and the two can
//	              never drift apart
//
// The denominator is a PROXY (manifest edits only — it undercounts decision
// moments, and captures at non-manifest moments can push the ratio over 100%),
// so the report must always carry raw counts, never just the percentage.

// isManifestDecisionEdit is the shared nudge/gauge classifier: it must agree
// with what nudgeReminder fires on, including the lockfile silence.
func TestIsManifestDecisionEdit(t *testing.T) {
	tests := []struct {
		name string
		tool string
		path string
		want bool
	}{
		{"edit go.mod", "Edit", "/r/go.mod", true},
		{"write package.json", "Write", "/r/web/package.json", true},
		{"multiedit Cargo.toml case-insensitive", "MultiEdit", "/r/Cargo.toml", true},
		{"edit source file", "Edit", "/r/main.go", false},
		{"bash touching go.mod is not an edit tool", "Bash", "/r/go.mod", false},
		{"lockfiles stay silent", "Edit", "/r/package-lock.json", false},
		{"empty path", "Edit", "", false},
		// Terraform / infra files — must agree with nudge_test.go cases (never-drift invariant).
		{"edit terraform main.tf", "Edit", "/r/terraform/main.tf", true},
		{"write tfvars", "Write", "/r/envs/prod/terraform.tfvars", true},
		{"tfstate is generated — silent", "Edit", "/r/terraform.tfstate", false},
		{"lock.hcl is generated — silent", "Edit", "/r/.terraform.lock.hcl", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isManifestDecisionEdit(tt.tool, tt.path); got != tt.want {
				t.Errorf("isManifestDecisionEdit(%q, %q) = %v, want %v", tt.tool, tt.path, got, tt.want)
			}
		})
	}
}

// writeSession writes one transcript fixture and returns its path.
func writeSession(t *testing.T, root, project, name string, lines []string) string {
	t.Helper()
	dir := filepath.Join(root, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

// The core scan: one session containing one genuine capture (plus a duplicate
// and a prose fake that must NOT count) and two manifest-edit signals (plus a
// source edit and a lockfile edit that must NOT count).
func TestScanCaptureRateCountsAndDedups(t *testing.T) {
	root := t.TempDir()
	writeSession(t, root, "-Users-x-repo1", "11111111-1111-1111-1111-111111111111.jsonl", []string{
		`{"type":"user","cwd":"/Users/x/repo1","timestamp":"2026-06-07T10:00:00Z","message":{"content":"start"}}`,
		// genuine capture (Form A: real tool_use block)
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__lema__record_decision","input":{"title":"Use pgvector","chosen":"pgvector"}}]}}`,
		// exact duplicate — dedups to one capture
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__lema__record_decision","input":{"title":"Use pgvector","chosen":"pgvector"}}]}}`,
		// prose fake: passes the substring gate, fails structural validation
		`{"type":"assistant","message":{"content":[{"type":"text","text":"please call record_decision with \"type\":\"tool_use\" semantics"}]}}`,
		// two real decision-shaped signals
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/Users/x/repo1/go.mod","old_string":"a","new_string":"b"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"/Users/x/repo1/web/package.json","content":"{}"}}]}}`,
		// non-signals: a source edit and a lockfile edit
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/Users/x/repo1/main.go","old_string":"a","new_string":"b"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/Users/x/repo1/package-lock.json","old_string":"a","new_string":"b"}}]}}`,
	})

	rep, err := scanCaptureRate(root, time.Time{})
	if err != nil {
		t.Fatalf("scanCaptureRate: %v", err)
	}
	if rep.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1", rep.Sessions)
	}
	if rep.Captures != 1 {
		t.Errorf("Captures = %d, want 1 (duplicate must dedup, prose fake must not count)", rep.Captures)
	}
	if rep.Signals != 2 {
		t.Errorf("Signals = %d, want 2 (source + lockfile edits must not count)", rep.Signals)
	}
	tally, ok := rep.ByRepo["repo1"]
	if !ok {
		t.Fatalf("ByRepo missing %q: %+v", "repo1", rep.ByRepo)
	}
	if tally.Captures != 1 || tally.Signals != 2 {
		t.Errorf("ByRepo[repo1] = %+v, want {Signals:2 Captures:1}", tally)
	}
}

// Sessions older than the cutoff are excluded (mtime windowing); a zero cutoff
// means all time. The gauge answers "is capture happening NOW" — stale history
// must not permanently depress the rate.
func TestScanCaptureRateWindowing(t *testing.T) {
	root := t.TempDir()
	p := writeSession(t, root, "-Users-x-old", "22222222-2222-2222-2222-222222222222.jsonl", []string{
		`{"type":"user","cwd":"/Users/x/old","timestamp":"2026-01-01T10:00:00Z","message":{"content":"start"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/Users/x/old/go.mod","old_string":"a","new_string":"b"}}]}}`,
	})
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	rep, err := scanCaptureRate(root, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("scanCaptureRate: %v", err)
	}
	if rep.Sessions != 0 || rep.Signals != 0 {
		t.Errorf("windowed scan = %+v, want zero sessions/signals (file mtime is 90d old)", rep)
	}

	all, err := scanCaptureRate(root, time.Time{})
	if err != nil {
		t.Fatalf("scanCaptureRate all-time: %v", err)
	}
	if all.Sessions != 1 || all.Signals != 1 {
		t.Errorf("all-time scan = %+v, want 1 session / 1 signal", all)
	}
}

// The report never reduces to a bare percentage: raw counts always present,
// and a zero-signal window prints n/a rather than a fake 0%.
func TestFormatCaptureRate(t *testing.T) {
	got := formatCaptureRate(captureRateReport{
		Sessions: 3, Signals: 2, Captures: 1,
		ByRepo: map[string]repoTally{"repo1": {Signals: 2, Captures: 1}},
	}, 30)
	for _, want := range []string{"1/2", "50.0%", "repo1"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatCaptureRate missing %q in:\n%s", want, got)
		}
	}

	zero := formatCaptureRate(captureRateReport{Sessions: 1}, 30)
	if !strings.Contains(zero, "n/a") {
		t.Errorf("zero-signal report must print n/a, got:\n%s", zero)
	}
	if strings.Contains(zero, "NaN") || strings.Contains(zero, "+Inf") {
		t.Errorf("zero-signal report leaks a NaN/Inf:\n%s", zero)
	}
}
