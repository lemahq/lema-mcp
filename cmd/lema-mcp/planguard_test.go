package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
)

// --- classifyAction: read the always-known actions array, never values ---

func TestClassifyAction(t *testing.T) {
	cases := []struct {
		name    string
		actions []string
		want    string
	}{
		{"create", []string{"create"}, "create"},
		{"update", []string{"update"}, "update"},
		{"delete", []string{"delete"}, "delete"},
		{"read", []string{"read"}, "read"},
		{"noop", []string{"no-op"}, "no-op"},
		{"replace delete-then-create", []string{"delete", "create"}, "replace"},
		{"replace create-then-delete", []string{"create", "delete"}, "replace"},
		{"empty is noop", nil, "no-op"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyAction(tc.actions); got != tc.want {
				t.Fatalf("classifyAction(%v) = %q, want %q", tc.actions, got, tc.want)
			}
		})
	}
}

// --- changedKeys: keys that differ, plus after_unknown keys; values never read ---

func TestChangedKeysValueDiff(t *testing.T) {
	ch := planChange{
		Before: map[string]any{"tier": "db-f1-micro", "region": "us-central1"},
		After:  map[string]any{"tier": "db-custom-2-4096", "region": "us-central1"},
	}
	got := changedKeys(ch)
	if !slices.Contains(got, "tier") {
		t.Fatalf("changedKeys = %v, want it to include the changed key 'tier'", got)
	}
	if slices.Contains(got, "region") {
		t.Fatalf("changedKeys = %v, must NOT include unchanged key 'region'", got)
	}
}

func TestChangedKeysIncludesAfterUnknown(t *testing.T) {
	// A "(known after apply)" attribute is marked in after_unknown. Its value is
	// unknowable, but the KEY is changing — include it (anchor on keys, not values).
	ch := planChange{
		Before:       map[string]any{},
		After:        map[string]any{},
		AfterUnknown: map[string]any{"self_link": true},
	}
	if got := changedKeys(ch); !slices.Contains(got, "self_link") {
		t.Fatalf("changedKeys = %v, want it to include after_unknown key 'self_link'", got)
	}
}

func TestChangedKeysIncludesRemovedKey(t *testing.T) {
	ch := planChange{
		Before: map[string]any{"deletion_protection": true},
		After:  map[string]any{},
	}
	if got := changedKeys(ch); !slices.Contains(got, "deletion_protection") {
		t.Fatalf("changedKeys = %v, want it to include removed key 'deletion_protection'", got)
	}
}

func TestChangedKeysSkipsSensitiveKeys(t *testing.T) {
	ch := planChange{
		Before: map[string]any{"password": "old"},
		After:  map[string]any{"password": "new"},
	}
	if got := changedKeys(ch); slices.Contains(got, "password") {
		t.Fatalf("changedKeys = %v, must NOT surface sensitive key 'password'", got)
	}
}

// --- planQuery: skip no-op/read; compose identity + changed keys otherwise ---

func TestPlanQuerySkipsNoopAndRead(t *testing.T) {
	for _, act := range []string{"no-op", "read"} {
		rc := resourceChange{
			Address: "google_storage_bucket.assets",
			Type:    "google_storage_bucket",
			Name:    "assets",
			Change:  planChange{Actions: []string{act}},
		}
		q, action, skip := planQuery(rc)
		if !skip {
			t.Fatalf("planQuery for %q action: skip = false, want true (nothing decided is reopened)", act)
		}
		if q != "" {
			t.Fatalf("planQuery for %q action: query = %q, want empty", act, q)
		}
		if action != act {
			t.Fatalf("planQuery for %q action: action = %q, want %q", act, action, act)
		}
	}
}

func TestPlanQueryComposesIdentityAndKeys(t *testing.T) {
	rc := resourceChange{
		Address: "google_sql_database_instance.primary",
		Type:    "google_sql_database_instance",
		Name:    "primary",
		Change: planChange{
			Actions: []string{"update"},
			Before:  map[string]any{"availability_type": "ZONAL"},
			After:   map[string]any{"availability_type": "REGIONAL"},
		},
	}
	q, action, skip := planQuery(rc)
	if skip {
		t.Fatalf("planQuery: skip = true, want false for an update")
	}
	if action != "update" {
		t.Fatalf("planQuery: action = %q, want update", action)
	}
	for _, want := range []string{"google_sql_database_instance", "primary", "availability_type"} {
		if !strings.Contains(q, want) {
			t.Fatalf("planQuery = %q, want it to contain %q", q, want)
		}
	}
}

// --- scanPlan: the TP/TN core, matching plan changes against closed atoms ---

func closedAtom(matchKey, note, ref string) source.Atom {
	return source.Atom{MatchKey: matchKey, ClosedNote: note, Ref: ref, Closed: true, Text: matchKey}
}

func TestScanPlanFlagsReopenedDecision(t *testing.T) {
	closed := []source.Atom{
		closedAtom("Aurora Postgres cluster",
			`do not propose "Aurora Postgres cluster": we standardized on plain RDS for cost`,
			"d_aaa111"),
	}
	plan := tfPlan{ResourceChanges: []resourceChange{{
		Address: "aws_rds_cluster.aurora",
		Type:    "aws_rds_cluster",
		Name:    "aurora",
		Change: planChange{
			Actions: []string{"update"},
			Before:  map[string]any{"engine": "aurora-mysql"},
			After:   map[string]any{"engine": "aurora-postgresql"},
		},
	}}}
	hits := scanPlan(plan, closed)
	if len(hits) != 1 {
		t.Fatalf("scanPlan: got %d hits, want 1 (the reopened Aurora decision)", len(hits))
	}
	if hits[0].Address != "aws_rds_cluster.aurora" {
		t.Fatalf("hit address = %q, want aws_rds_cluster.aurora", hits[0].Address)
	}
	if hits[0].Action != "update" {
		t.Fatalf("hit action = %q, want update", hits[0].Action)
	}
	if hits[0].Atom.Ref != "d_aaa111" {
		t.Fatalf("hit atom ref = %q, want d_aaa111 (cite the recorded decision)", hits[0].Atom.Ref)
	}
}

func TestScanPlanSilentOnUnrelatedChange(t *testing.T) {
	closed := []source.Atom{
		closedAtom("Aurora Postgres cluster", "do not propose Aurora", "d_aaa111"),
	}
	plan := tfPlan{ResourceChanges: []resourceChange{{
		Address: "google_storage_bucket.assets",
		Type:    "google_storage_bucket",
		Name:    "assets",
		Change: planChange{
			Actions: []string{"create"},
			After:   map[string]any{"location": "US"},
		},
	}}}
	if hits := scanPlan(plan, closed); len(hits) != 0 {
		t.Fatalf("scanPlan: got %d hits, want 0 (unrelated change must stay silent)", len(hits))
	}
}

func TestScanPlanEmptyClosedReturnsNil(t *testing.T) {
	plan := tfPlan{ResourceChanges: []resourceChange{{
		Address: "aws_rds_cluster.aurora",
		Type:    "aws_rds_cluster",
		Name:    "aurora",
		Change:  planChange{Actions: []string{"update"}, After: map[string]any{"engine": "x"}},
	}}}
	if hits := scanPlan(plan, nil); hits != nil {
		t.Fatalf("scanPlan with no closed atoms = %v, want nil (fail-open: nothing settled to reopen)", hits)
	}
}

// --- emitReview: advisory markdown citing the recorded decision, never invented ---

func TestEmitReviewRendersDecision(t *testing.T) {
	hits := []planConflict{{
		Address: "aws_rds_cluster.aurora",
		Action:  "update",
		Atom:    closedAtom("Aurora Postgres cluster", "do not propose Aurora: cost", "d_aaa111"),
	}}
	md := emitReview(hits)
	for _, want := range []string{"aws_rds_cluster.aurora", "update", "do not propose Aurora: cost", "d_aaa111"} {
		if !strings.Contains(md, want) {
			t.Fatalf("emitReview output missing %q.\n---\n%s", want, md)
		}
	}
}

func TestEmitReviewEmptyWhenNoHits(t *testing.T) {
	if md := emitReview(nil); md != "" {
		t.Fatalf("emitReview(nil) = %q, want empty (silent when clean)", md)
	}
}

// --- planExitCode: advisory v1 never blocks a deploy, even on a hit ---

func TestPlanExitCodeAdvisoryAlwaysZero(t *testing.T) {
	hits := []planConflict{{Address: "x", Action: "update", Atom: closedAtom("k", "n", "d_x")}}
	if code := planExitCode(hits, planGuardModeContext); code != 0 {
		t.Fatalf("planExitCode in context mode = %d, want 0 (advisory: never block apply in v1)", code)
	}
}

// --- planGuardRun: the shell. Fail-open is the load-bearing contract ---

func TestPlanGuardRunFailOpenMissingPlan(t *testing.T) {
	var buf bytes.Buffer
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	if code := planGuardRun([]string{"--plan", missing}, &buf); code != 0 {
		t.Fatalf("planGuardRun on missing plan = %d, want 0 (fail-open)", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("planGuardRun on missing plan emitted %q, want nothing", buf.String())
	}
}

func TestPlanGuardRunFailOpenMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	planFile := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(planFile, []byte("{ this is not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := planGuardRun([]string{"--plan", planFile}, &buf); code != 0 {
		t.Fatalf("planGuardRun on malformed plan = %d, want 0 (fail-open)", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("planGuardRun on malformed plan emitted %q, want nothing", buf.String())
	}
}

const seededDecisions = `{"id":"d_mongo1","ts":"2026-06-19T00:00Z","title":"Primary store","chosen":"Postgres","rejected":[{"option":"MongoDB document store","why":"relational fit"}],"status":"accepted"}
{"id":"d_redis1","ts":"2026-06-19T00:00Z","title":"Cache","chosen":"in-process","rejected":[{"option":"Redis cache layer","why":"avoid an extra service"}],"status":"accepted"}
{"id":"d_mysql1","ts":"2026-06-19T00:00Z","title":"SQL engine","chosen":"Postgres on Cloud SQL","rejected":[{"option":"MySQL engine","why":"JSONB and partial indexes won it"}],"status":"accepted"}
`

func writeSeed(t *testing.T) (capFile string) {
	t.Helper()
	capFile = filepath.Join(t.TempDir(), "decisions.jsonl")
	if err := os.WriteFile(capFile, []byte(seededDecisions), 0o600); err != nil {
		t.Fatal(err)
	}
	return capFile
}

func writePlan(t *testing.T, body string) string {
	t.Helper()
	planFile := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return planFile
}

func TestPlanGuardRunFlagsSeededDecision(t *testing.T) {
	capFile := writeSeed(t)
	// A plan that switches the SQL engine back to MySQL — reopens d_mysql1.
	planFile := writePlan(t, `{"resource_changes":[
		{"address":"google_sql_database_instance.mysql","type":"google_sql_database_instance","name":"mysql",
		 "change":{"actions":["update"],"before":{"tier":"db-f1-micro"},"after":{"tier":"db-custom-2-4096"}}}
	]}`)
	var buf bytes.Buffer
	code := planGuardRun([]string{"--plan", planFile, "--capture-file", capFile}, &buf)
	if code != 0 {
		t.Fatalf("planGuardRun = %d, want 0 (advisory)", code)
	}
	out := buf.String()
	if !strings.Contains(out, "google_sql_database_instance.mysql") {
		t.Fatalf("output missing the flagged address.\n---\n%s", out)
	}
	if !strings.Contains(out, "d_mysql1") {
		t.Fatalf("output must cite the recorded decision d_mysql1.\n---\n%s", out)
	}
	if !strings.Contains(out, "MySQL engine") {
		t.Fatalf("output must surface the recorded ClosedNote (MySQL engine), never an invented why.\n---\n%s", out)
	}
}

func TestPlanGuardRunSilentOnUnrelatedPlan(t *testing.T) {
	capFile := writeSeed(t)
	// A routine bucket create — touches nothing settled.
	planFile := writePlan(t, `{"resource_changes":[
		{"address":"google_storage_bucket.assets","type":"google_storage_bucket","name":"assets",
		 "change":{"actions":["create"],"before":null,"after":{"location":"US"}}}
	]}`)
	var buf bytes.Buffer
	code := planGuardRun([]string{"--plan", planFile, "--capture-file", capFile}, &buf)
	if code != 0 {
		t.Fatalf("planGuardRun = %d, want 0", code)
	}
	if buf.Len() != 0 {
		t.Fatalf("planGuardRun on unrelated plan emitted %q, want nothing (precision)", buf.String())
	}
}
