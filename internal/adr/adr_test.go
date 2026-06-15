package adr

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func writeADR(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func TestParseFile_FrontmatterAndBody(t *testing.T) {
	dir := t.TempDir()
	writeADR(t, dir, "0016-adopt-postgres.md", `---
title: Adopt Postgres
status: accepted
date: 2026-05-17
authors:
  - andrew
tags: [data, foundational]
---

# Adopt Postgres

We chose Postgres because of ACID guarantees.
`)
	adrs, err := ParseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(adrs) != 1 {
		t.Fatalf("got %d adrs, want 1", len(adrs))
	}
	a := adrs[0]
	if a.Number != 16 {
		t.Errorf("Number = %d, want 16", a.Number)
	}
	if a.Slug != "adopt-postgres" {
		t.Errorf("Slug = %q, want adopt-postgres", a.Slug)
	}
	if a.Title != "Adopt Postgres" {
		t.Errorf("Title = %q", a.Title)
	}
	if a.Status != "accepted" {
		t.Errorf("Status = %q", a.Status)
	}
	if !reflect.DeepEqual(a.Tags, []string{"data", "foundational"}) {
		t.Errorf("Tags = %v", a.Tags)
	}
	if !strings.Contains(a.Body, "ACID guarantees") {
		t.Errorf("Body missing expected text: %q", a.Body)
	}
}

// TestParseFile_ProjectSlug pins the optional `project:` frontmatter key
// (ADR-0061 §8 Phase 1b): present → carried through so the hosted import can
// resolve it to a project_id at ingest (skip-on-miss); absent → empty string,
// never an error. This is the leg that feeds hosted project timelines from
// repo ADR files.
//
// The malformed-form cases (list, mapping, number) pin the flexString
// leniency: `project:` is an optional enrichment field with skip-on-miss
// semantics, so a malformed value must degrade to a miss ("") — a parse error
// here would propagate through ListAndParseADRs and 502 the ENTIRE repo
// import, the same hazard flexRef prevents for superseded_by.
func TestParseFile_ProjectSlug(t *testing.T) {
	dir := t.TempDir()
	writeADR(t, dir, "0030-with-project.md", `---
title: With project
status: accepted
project: payments-replatform
---
body
`)
	writeADR(t, dir, "0031-without-project.md", `---
title: Without project
status: accepted
---
body
`)
	writeADR(t, dir, "0032-list-project.md", `---
title: List project
status: accepted
project: [payments-replatform]
---
body
`)
	writeADR(t, dir, "0033-null-project.md", `---
title: Null project
status: accepted
project: null
---
body
`)
	writeADR(t, dir, "0034-mapping-project.md", `---
title: Mapping project
status: accepted
project: {slug: payments-replatform}
---
body
`)
	writeADR(t, dir, "0035-number-project.md", `---
title: Number project
status: accepted
project: 12
---
body
`)
	adrs, err := ParseDir(dir)
	if err != nil {
		t.Fatalf("ParseDir must not fail on any project: form — an optional enrichment field must never block a whole repo's import: %v", err)
	}
	if len(adrs) != 6 {
		t.Fatalf("got %d adrs, want 6", len(adrs))
	}
	if adrs[0].Project != "payments-replatform" {
		t.Errorf("scalar Project = %q, want payments-replatform", adrs[0].Project)
	}
	if adrs[1].Project != "" {
		t.Errorf("Project = %q, want empty when frontmatter has no project key", adrs[1].Project)
	}
	if adrs[2].Project != "payments-replatform" {
		t.Errorf("list-form Project = %q, want payments-replatform (natural authoring mistake — siblings tags/supersedes ARE lists — must resolve, not error)", adrs[2].Project)
	}
	if adrs[3].Project != "" {
		t.Errorf("null Project = %q, want empty (explicit null is a miss, not an error)", adrs[3].Project)
	}
	if adrs[4].Project != "" {
		t.Errorf("mapping Project = %q, want empty (unsupported shape degrades to a miss so the import stays safe)", adrs[4].Project)
	}
	if adrs[5].Project != "12" {
		t.Errorf("number Project = %q, want \"12\" (yaml coerces the scalar; a harmless miss at resolution time)", adrs[5].Project)
	}
}

// TestParseFile_ZeroPaddedRefsAreDecimalNotOctal pins the bug that running the
// spike surfaced: YAML resolves a zero-padded scalar like 0012 as OCTAL (= 10),
// which would silently point ADR edges at the wrong decisions. References must
// be base-10 ADR numbers. This test fails if the parser ever reverts edge
// fields from []string-then-Atoi back to []int.
func TestParseFile_ZeroPaddedRefsAreDecimalNotOctal(t *testing.T) {
	dir := t.TempDir()
	writeADR(t, dir, "0020-edges.md", `---
title: Edge test
status: accepted
depends_on: [0007, 0012, 0013, 0014]
supersedes: [0016]
superseded_by: 0099
related_to: []
---
body
`)
	adrs, err := ParseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := adrs[0]
	if got, want := a.DependsOn, []int{7, 12, 13, 14}; !reflect.DeepEqual(got, want) {
		t.Errorf("DependsOn = %v, want %v (octal regression?)", got, want)
	}
	if got, want := a.Supersedes, []int{16}; !reflect.DeepEqual(got, want) {
		t.Errorf("Supersedes = %v, want %v", got, want)
	}
	if a.SupersededBy == nil || *a.SupersededBy != 99 {
		t.Errorf("SupersededBy = %v, want 99", a.SupersededBy)
	}
}

func TestParseFile_NullSupersededByIsNil(t *testing.T) {
	dir := t.TempDir()
	writeADR(t, dir, "0001-x.md", "---\ntitle: X\nstatus: proposed\nsuperseded_by: null\n---\nbody")
	adrs, _ := ParseDir(dir)
	if adrs[0].SupersededBy != nil {
		t.Errorf("SupersededBy = %v, want nil", adrs[0].SupersededBy)
	}
}

// TestParseFile_SupersededByAcceptsSequence pins the leniency that lets a
// real dogfood ADR (0001 uses `superseded_by: [2]`) parse instead of crashing
// ParseDir for the whole directory. The list form resolves to its first
// element, and the octal-safety of the scalar path must hold through the
// sequence path too (a zero-padded `[0009]` is 9, not 11).
func TestParseFile_SupersededByAcceptsSequence(t *testing.T) {
	dir := t.TempDir()
	writeADR(t, dir, "0001-x.md", "---\ntitle: X\nstatus: superseded\nsuperseded_by: [2]\n---\nbody")
	writeADR(t, dir, "0002-y.md", "---\ntitle: Y\nstatus: superseded\nsuperseded_by: [0009]\n---\nbody")
	adrs, err := ParseDir(dir)
	if err != nil {
		t.Fatalf("ParseDir must not fail on a list-form superseded_by: %v", err)
	}
	if adrs[0].SupersededBy == nil || *adrs[0].SupersededBy != 2 {
		t.Errorf("0001 SupersededBy = %v, want 2", adrs[0].SupersededBy)
	}
	if adrs[1].SupersededBy == nil || *adrs[1].SupersededBy != 9 {
		t.Errorf("0002 SupersededBy = %v, want 9 (octal-safe through sequence path)", adrs[1].SupersededBy)
	}
}

func TestParseFile_NoFrontmatterFallsBackToFirstH1(t *testing.T) {
	dir := t.TempDir()
	writeADR(t, dir, "0001-no-fm.md", "# Just a heading\n\nsome text\n")
	adrs, _ := ParseDir(dir)
	if adrs[0].Title != "Just a heading" {
		t.Errorf("Title = %q, want 'Just a heading'", adrs[0].Title)
	}
}

// A horizontal rule in the body must not be mistaken for the closing
// frontmatter fence — the fence is the first `---` line, and body content
// after a later `---` must survive.
func TestSplitFrontmatter_BodyHorizontalRuleSurvives(t *testing.T) {
	dir := t.TempDir()
	writeADR(t, dir, "0002-hr.md", `---
title: HR test
status: accepted
---

Intro paragraph.

---

After a horizontal rule.
`)
	adrs, _ := ParseDir(dir)
	a := adrs[0]
	if a.Title != "HR test" {
		t.Errorf("Title = %q", a.Title)
	}
	if !strings.Contains(a.Body, "After a horizontal rule") {
		t.Errorf("body lost content after horizontal rule: %q", a.Body)
	}
}

func TestParseDir_SkipsNonADRFilesAndSortsByNumber(t *testing.T) {
	dir := t.TempDir()
	writeADR(t, dir, "0005-five.md", "---\ntitle: Five\nstatus: proposed\n---\nx")
	writeADR(t, dir, "0002-two.md", "---\ntitle: Two\nstatus: proposed\n---\nx")
	writeADR(t, dir, "README.md", "# Readme\nnot an adr")
	writeADR(t, dir, "template.md", "---\ntitle: Template\n---\nx")
	adrs, err := ParseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(adrs) != 2 {
		t.Fatalf("got %d adrs, want 2 (README.md and template.md must be skipped)", len(adrs))
	}
	if adrs[0].Number != 2 || adrs[1].Number != 5 {
		t.Errorf("not sorted by number: got %d then %d", adrs[0].Number, adrs[1].Number)
	}
}

func TestParseFile_BOMStripped(t *testing.T) {
	dir := t.TempDir()
	bom := string([]byte{0xEF, 0xBB, 0xBF})
	writeADR(t, dir, "0003-bom.md", bom+"---\ntitle: BOM\nstatus: accepted\n---\nbody")
	adrs, _ := ParseDir(dir)
	if adrs[0].Title != "BOM" {
		t.Errorf("BOM not stripped — Title = %q, want BOM", adrs[0].Title)
	}
}

// ParseDirMatching with a looser pattern indexes non-canonical ADR names (the
// local wedge pointed at MADR/adr-tools-style repos): the number falls back to
// the first digit-run and the slug to the extension-less name. Strict ParseDir
// must still skip them, so the hosted/import path is unchanged.
func TestParseDirMatching_LooserPatternAndNumberFallback(t *testing.T) {
	dir := t.TempDir()
	writeADR(t, dir, "001-use-madr.md", "# Use MADR\nWe will use MADR.")                                  // 3-digit, non-canonical
	writeADR(t, dir, "0007-event-sourcing.md", "---\ntitle: Event sourcing\nstatus: accepted\n---\nbody") // canonical

	// Strict ParseDir skips the 3-digit file — the hosted matcher is unchanged.
	strict, err := ParseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(strict) != 1 || strict[0].Number != 7 {
		t.Fatalf("strict ParseDir got %d adrs (want 1, ADR-7) — strict matcher changed?", len(strict))
	}

	// Looser pattern picks up both; the 3-digit number is recovered via fallback.
	all, err := ParseDirMatching(dir, regexp.MustCompile(`^\d{3,4}[-_].+\.md$`))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("looser ParseDirMatching got %d adrs, want 2", len(all))
	}
	if all[0].Number != 1 || all[0].Slug != "001-use-madr" || all[0].Title != "Use MADR" {
		t.Errorf("non-canonical parse = #%d %q %q, want #1 001-use-madr \"Use MADR\"", all[0].Number, all[0].Slug, all[0].Title)
	}
}

// Foreign repos rarely use lema's YAML frontmatter — MADR 2.x puts status in a
// metadata bullet under the H1 (`- Status: accepted`), Nygard/adr-tools in a
// `## Status` section. Without sniffing those, every imported foreign ADR
// landed status="" → proposed → "in deliberation" in the UI (48/49 on the
// 2026-06-12 fxa stranger walk). Frontmatter, when present, always wins.
func TestParseBytes_StatusSniffedFromBody(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"madr dash bullet", "# T\n\n- Status: accepted\n- Deciders: a, b\n\n## Context\n", "accepted"},
		{"madr star bullet uppercase", "# T\n\n* Status: REJECTED\n\n## Context\n", "rejected"},
		{"madr superseded by link", "# T\n\n- Status: superseded by [ADR-0017](0017-switch.md)\n\n## Context\n", "superseded"},
		{"nygard status section", "# T\n\n## Status\n\nAccepted\n\n## Context\n\ntext\n", "accepted"},
		{"nygard section superseded link", "# T\n\n## Status\n\nSuperseded by [2. New](0002-new.md)\n\n## Context\n", "superseded"},
		{"template placeholder is ambiguous", "# T\n\n* Status: [proposed | rejected | accepted | deprecated | superseded by [ADR-0005](0005-example.md)] <!-- optional -->\n\n## Context\n", ""},
		{"unrecognized value abstains", "# T\n\n- Status: Final\n\n## Context\n", ""},
		{"bullet after first section ignored", "# T\n\n## Context\n\n- Status: rejected was the old state\n", ""},
		{"prose status line ignored", "# T\n\n## Context\n\nStatus: the proposal was rejected upstream.\n", ""},
		{"no marker at all", "# T\n\n## Context\n\nWe decided things.\n", ""},
		{"empty status section", "# T\n\n## Status\n\n## Context\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, err := ParseBytes([]byte(c.raw), "0001-x.md", "")
			if err != nil {
				t.Fatal(err)
			}
			if a.Status != c.want {
				t.Errorf("Status = %q, want %q", a.Status, c.want)
			}
		})
	}
}

// Explicit frontmatter status is authoritative — body sniffing only fills the
// gap when frontmatter says nothing.
func TestParseBytes_FrontmatterStatusBeatsBodySniff(t *testing.T) {
	raw := "---\ntitle: T\nstatus: proposed\n---\n\n- Status: accepted\n\n## Status\n\nAccepted\n"
	a, err := ParseBytes([]byte(raw), "0002-x.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "proposed" {
		t.Errorf("Status = %q, want frontmatter's 'proposed'", a.Status)
	}
}
