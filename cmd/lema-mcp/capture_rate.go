package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// capture_rate.go is the `lema-mcp capture-rate` subcommand — the capture-rate
// gauge, the heartbeat instrument of the judgment layer. An empty graph saves
// nothing; this is how we know whether decisions are actually being captured,
// before building more capture engineering on top (the "instrument before the
// thing it instruments" rule).
//
// The metric contract:
//
//	numerator   = GENUINE record_decision calls in the local agent transcripts —
//	              structurally validated by extractDecision (a "record_decision"
//	              substring is prose noise in a repo that authors its own
//	              tooling), deduped per session by the same ref/title key the
//	              Sessions surface uses.
//	denominator = decision-shaped moments — dependency-manifest edits, classified
//	              by isManifestDecisionEdit, the SAME predicate the capture nudge
//	              fires on (ADR-0054). Gauge and nudge cannot drift apart.
//
// Honesty constraints, carried in the output: the denominator is a PROXY. It
// undercounts decision moments (decisions happen outside manifest edits), and a
// capture at a non-manifest moment can push the ratio over 100%. So the report
// always leads with raw counts; the percentage never appears without them, and
// a zero-signal window prints n/a rather than a fake 0%.
//
// OSS SEAM (ADR-0034): public binary — stdlib + same-package helpers only.
// PRIVACY: emits ONLY counts and repo basenames; no prompt text, no transcript
// content, no queries. Oversized transcript lines (> scannerBufCap) are drained
// and skipped exactly as the Sessions scan does — a giant Write body containing
// a manifest path may be missed, a recorded undercount, not a crash.

// repoTally is one repo's slice of the gauge.
type repoTally struct {
	Signals  int `json:"signals"`
	Captures int `json:"captures"`
}

// captureRateReport is the gauge's result over one scan window.
type captureRateReport struct {
	Sessions int                  `json:"sessions"`
	Signals  int                  `json:"signals"`
	Captures int                  `json:"captures"`
	ByRepo   map[string]repoTally `json:"by_repo"`
}

// scanCaptureRate walks every <project>/<session>.jsonl under root, skipping
// files whose mtime is before cutoff (zero cutoff = all time), and tallies
// captures and signals. Unreadable files are skipped (fail-open: the gauge
// reports what it can see, it never blocks on a single bad transcript).
func scanCaptureRate(root string, cutoff time.Time) (captureRateReport, error) {
	rep := captureRateReport{ByRepo: map[string]repoTally{}}
	projects, err := os.ReadDir(root)
	if err != nil {
		return rep, fmt.Errorf("read sessions root %s: %w", root, err)
	}
	for _, proj := range projects {
		if !proj.IsDir() {
			continue
		}
		dir := filepath.Join(root, proj.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			if !cutoff.IsZero() && info.ModTime().Before(cutoff) {
				continue
			}
			repo, signals, captures, err := scanCaptureRateFile(filepath.Join(dir, f.Name()))
			if err != nil {
				continue
			}
			rep.Sessions++
			rep.Signals += signals
			rep.Captures += captures
			t := rep.ByRepo[repo]
			t.Signals += signals
			t.Captures += captures
			rep.ByRepo[repo] = t
		}
	}
	return rep, nil
}

// scanCaptureRateFile streams one transcript and returns its repo label plus
// signal/capture counts. Substring-gated exactly like scanSession: a line is
// JSON-parsed only when a cheap contains test says it could matter, so the
// giant assistant/tool_result bodies are skipped unparsed.
func scanCaptureRateFile(path string) (repo string, signals, captures int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, scannerBufCap)
	var shortestCwd string
	decSeen := map[string]bool{}

	for {
		line, oversized, eof := readScanLine(br)
		if oversized {
			continue // drained and skipped, scan continues (recorded undercount)
		}
		if eof && len(line) == 0 {
			break
		}
		if len(line) == 0 {
			continue
		}

		hasToolUse := bytesContains(line, `"type":"tool_use"`)

		// Decision gate — mirrors scanSession: the substring is necessary but not
		// sufficient; extractDecision's structural validation is the real filter.
		maybeFormA := hasToolUse && bytesContains(line, "record_decision")
		maybeFormB := bytesContains(line, `"attributionMcpTool":"record_decision"`)
		// Signal gate: a tool_use block carrying a file_path could be a manifest
		// edit; isManifestDecisionEdit decides after parse.
		maybeSignal := hasToolUse && bytesContains(line, `"file_path"`)
		needCwd := shortestCwd == "" && bytesContains(line, `"cwd"`)

		if !maybeFormA && !maybeFormB && !maybeSignal && !needCwd {
			continue
		}

		var rec jsonlRecord
		if json.Unmarshal(line, &rec) != nil {
			continue
		}

		if rec.Cwd != "" && (shortestCwd == "" || len(rec.Cwd) < len(shortestCwd)) {
			shortestCwd = rec.Cwd
		}

		if maybeFormA || maybeFormB {
			if d, ok := extractDecision(rec); ok {
				key := d.Ref
				if key == "" {
					key = strings.ToLower(strings.TrimSpace(d.Title))
				}
				if key != "" && !decSeen[key] {
					decSeen[key] = true
					captures++
				}
			}
		}

		if maybeSignal && rec.Type == "assistant" && len(rec.Message) > 0 {
			var msg assistantMessage
			if json.Unmarshal(rec.Message, &msg) == nil {
				for _, b := range msg.Content {
					if b.Type != "tool_use" {
						continue
					}
					var in struct {
						FilePath string `json:"file_path"`
					}
					if json.Unmarshal(b.Input, &in) != nil {
						continue
					}
					if isManifestDecisionEdit(b.Name, in.FilePath) {
						signals++
					}
				}
			}
		}

		if eof {
			break
		}
	}

	if shortestCwd != "" {
		repo = filepath.Base(shortestCwd)
	} else {
		repo = deFlattenProjectDir(path)
	}
	return repo, signals, captures, nil
}

// formatCaptureRate renders the human report. Raw counts always lead; the
// percentage never appears alone; zero signals prints n/a (not 0%).
func formatCaptureRate(rep captureRateReport, days int) string {
	window := "all time"
	if days > 0 {
		window = fmt.Sprintf("last %d days", days)
	}
	rate := "n/a (no decision-shaped moments in the window)"
	if rep.Signals > 0 {
		rate = fmt.Sprintf("%.1f%% (%d/%d)", 100*float64(rep.Captures)/float64(rep.Signals), rep.Captures, rep.Signals)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "lema capture-rate · %s · %d sessions\n\n", window, rep.Sessions)
	fmt.Fprintf(&b, "  decision signals   %d   (dependency-manifest edits — the moments the nudge classifies)\n", rep.Signals)
	fmt.Fprintf(&b, "  genuine captures   %d   (structurally-validated record_decision calls, deduped per session)\n", rep.Captures)
	fmt.Fprintf(&b, "  capture rate       %s\n", rate)

	if len(rep.ByRepo) > 0 {
		repos := make([]string, 0, len(rep.ByRepo))
		for r := range rep.ByRepo {
			repos = append(repos, r)
		}
		sort.Slice(repos, func(i, j int) bool {
			ti, tj := rep.ByRepo[repos[i]], rep.ByRepo[repos[j]]
			if ti.Signals+ti.Captures != tj.Signals+tj.Captures {
				return ti.Signals+ti.Captures > tj.Signals+tj.Captures
			}
			return repos[i] < repos[j]
		})
		b.WriteString("\n  by repo (signals or captures only):\n")
		for _, r := range repos {
			t := rep.ByRepo[r]
			if t.Signals == 0 && t.Captures == 0 {
				continue
			}
			fmt.Fprintf(&b, "    %-24s %d captures / %d signals\n", r, t.Captures, t.Signals)
		}
	}

	b.WriteString("\n  caveats: the denominator is a proxy (manifest edits only) — it undercounts\n")
	b.WriteString("  decision moments, and captures at non-manifest moments can push the rate\n")
	b.WriteString("  over 100%. Read the raw counts, not just the percentage. Self-measured.\n")
	return b.String()
}

// runCaptureRate is the subcommand entry point.
func runCaptureRate(args []string) error {
	fs := flag.NewFlagSet("capture-rate", flag.ContinueOnError)
	days := fs.Int("days", 30, "window in days by transcript mtime (0 = all time)")
	rootFlag := fs.String("root", "", "sessions root (default: the local agent transcript store)")
	jsonOut := fs.Bool("json", false, "emit the report as JSON on stdout instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	root := *rootFlag
	if root == "" {
		var err error
		root, err = defaultSessionsRoot()
		if err != nil {
			return err
		}
	}

	var cutoff time.Time
	if *days > 0 {
		cutoff = time.Now().Add(-time.Duration(*days) * 24 * time.Hour)
	}

	rep, err := scanCaptureRate(root, cutoff)
	if err != nil {
		return err
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return err
		}
	} else {
		fmt.Print(formatCaptureRate(rep, *days))
	}

	// Accumulate onto the usage-log channel (same file logUsage writes) so the
	// gauge has a history without its own store. Best-effort: a bad path is
	// reported to stderr and never fails the report.
	if p := os.Getenv("LEMA_USAGE_LOG"); p != "" {
		line, _ := json.Marshal(map[string]any{
			"ts":       time.Now().UTC().Format(time.RFC3339),
			"tool":     "capture-rate",
			"days":     *days,
			"sessions": rep.Sessions,
			"signals":  rep.Signals,
			"captures": rep.Captures,
		})
		f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "lema-mcp: LEMA_USAGE_LOG rejected: %v\n", err)
		} else {
			fmt.Fprintln(f, string(line))
			f.Close()
		}
	}
	return nil
}
