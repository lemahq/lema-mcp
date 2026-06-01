// Command lema-mcp is the lema MCP server — the local, DB-less wedge an agent
// queries. It parses a repo's docs/adr/ on disk and serves four tools over stdio
// with no account, no database, and no network: search_decisions returns the
// most relevant atomic claims (lexical over ADR sections, the ADR-0025 §4
// response contract); get_decision / list_decisions / get_decision_graph serve
// whole decisions and the typed-edge graph for drill-down; record_decision /
// check_decided capture decisions at deliberation to a local store and enforce
// never-reopen (ADR-0042). The hosted tier (its
// own Cloud Run service, ADR-0033 §7) serves the same four-tool contract over
// the pgvector atom layer behind a DB-backed DecisionSource.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lemahq/lema-mcp/internal/adr"
	"github.com/lemahq/lema-mcp/internal/openspec"
	"github.com/lemahq/lema-mcp/internal/source"
)

var (
	src         source.DecisionSource // the four read tools go through this seam
	capture     *source.CaptureStore  // local decision-capture store (ADR-0042): record_decision writes, search/check_decided enforce
	repoName    = "local"             // identifier shown in search results
	usageLog    *os.File
	questionLog *os.File
)

// logUsage records each tool call and the approximate token size of the context
// returned — the input half of any tokens-saved measurement. Written to stderr
// so it never pollutes the stdio MCP protocol stream on stdout.
func logUsage(tool, query string, results int, payload any) {
	approxTokens := 0
	if b, err := json.Marshal(payload); err == nil {
		approxTokens = len(b) / 4
	}
	line, _ := json.Marshal(map[string]any{
		"ts":            time.Now().UTC().Format(time.RFC3339),
		"tool":          tool,
		"query":         query,
		"results":       results,
		"approx_tokens": approxTokens,
	})
	fmt.Fprintln(os.Stderr, "lema-mcp usage "+string(line))
	if usageLog != nil {
		fmt.Fprintln(usageLog, string(line))
	}
}

type listInput struct {
	Status string `json:"status,omitempty" jsonschema:"filter by status: proposed, accepted, superseded, deprecated, rejected. empty for all"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max results (default 50)"`
}
type listOutput struct {
	Decisions []source.Summary `json:"decisions"`
}

type getInput struct {
	Number int `json:"number" jsonschema:"the ADR number, e.g. 16"`
}
type getOutput struct {
	Decision source.Detail `json:"decision"`
}

type searchInput struct {
	Query     string `json:"query" jsonschema:"natural-language question about this repo's decisions"`
	K         int    `json:"k,omitempty" jsonschema:"max atoms to consider (default 8)"`
	MaxTokens int    `json:"max_tokens,omitempty" jsonschema:"token budget for the returned atoms (default 1500)"`
}

// searchOutput is the ADR-0025 §4 response contract: shared fields once, a
// minimal per-atom payload, and a truncation flag when the budget clipped results.
type searchOutput struct {
	Repo       string        `json:"repo"`
	Claims     []source.Atom `json:"claims"`
	TokensUsed int           `json:"tokens_used"`
	Usage      localUsage    `json:"usage"`
	Truncated  bool          `json:"truncated"`
}

type graphInput struct {
	Number int `json:"number" jsonschema:"the ADR number to start from"`
	Depth  int `json:"depth,omitempty" jsonschema:"edge-traversal depth (default 1, max 5)"`
}
type graphOutput struct {
	Graph source.Graph `json:"graph"`
}

func listDecisions(ctx context.Context, _ *mcp.CallToolRequest, in listInput) (*mcp.CallToolResult, listOutput, error) {
	ds, err := src.List(ctx, in.Status, in.Limit)
	if err != nil {
		return nil, listOutput{}, err
	}
	out := listOutput{Decisions: ds}
	logUsage("list_decisions", in.Status, len(ds), out)
	return nil, out, nil
}

func getDecision(ctx context.Context, _ *mcp.CallToolRequest, in getInput) (*mcp.CallToolResult, getOutput, error) {
	d, err := src.Get(ctx, in.Number)
	if err != nil {
		return nil, getOutput{}, err
	}
	out := getOutput{Decision: d}
	logUsage("get_decision", fmt.Sprintf("#%d", in.Number), 1, out)
	return nil, out, nil
}

// searchDecisions returns atomic claims via the DecisionSource (lexical locally,
// hybrid retrieval in the hosted backend), bounded by max_tokens. This is the
// token-efficient surface: tight, sourced atoms instead of whole-ADR bodies.
func searchDecisions(ctx context.Context, _ *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
	k := in.K
	if k <= 0 {
		k = 8
	}
	atoms, err := src.Search(ctx, in.Query, k)
	if err != nil {
		return nil, searchOutput{}, err
	}
	// Merge this repo's captured decisions (ADR-0042): CLOSED matches lead — an
	// agent about to re-propose a killed or superseded option must see it first —
	// then the ADR atoms, then any other captured decisions.
	if capture != nil {
		var closed, open []source.Atom
		for _, a := range capture.Search(in.Query, k) {
			if a.Closed {
				closed = append(closed, a)
			} else {
				open = append(open, a)
			}
		}
		atoms = append(append(closed, atoms...), open...)
	}
	budget := in.MaxTokens
	if budget <= 0 {
		budget = 1500
	}
	kept, used, truncated := fitBudget(atoms, budget)
	out := searchOutput{Repo: repoName, Claims: kept, TokensUsed: used, Usage: localSearchROI(ctx, kept), Truncated: truncated}
	logUsage("search_decisions", in.Query, len(kept), out)
	logQuestion(in.Query, kept)
	return nil, out, nil
}

func getDecisionGraph(ctx context.Context, _ *mcp.CallToolRequest, in graphInput) (*mcp.CallToolResult, graphOutput, error) {
	g, err := src.Graph(ctx, in.Number, in.Depth)
	if err != nil {
		return nil, graphOutput{}, err
	}
	out := graphOutput{Graph: g}
	logUsage("get_decision_graph", fmt.Sprintf("#%d", in.Number), len(g.Nodes), out)
	return nil, out, nil
}

// fitBudget keeps the highest-ranked atoms whose cumulative token estimate fits
// the budget, flagging truncation when more relevant atoms existed.
func fitBudget(atoms []source.Atom, budget int) ([]source.Atom, int, bool) {
	used := 0
	kept := make([]source.Atom, 0, len(atoms))
	for _, a := range atoms {
		t := (len(a.Text) + 3) / 4
		if used+t > budget && len(kept) > 0 {
			return kept, used, true
		}
		kept = append(kept, a)
		used += t
	}
	return kept, used, false
}

// logQuestion appends the agent's query to a local question log (the gap-
// flywheel substrate, ADR-0025 §1/§7) when LEMA_QUESTION_LOG is set. Local mode
// has no database; a query asked repeatedly with no good match is still a ranked
// gap, which the hosted tier can ingest later.
func logQuestion(query string, atoms []source.Atom) {
	if questionLog == nil {
		return
	}
	ids := make([]string, 0, len(atoms))
	for _, a := range atoms {
		ids = append(ids, a.ID)
	}
	line, _ := json.Marshal(map[string]any{
		"ts":                time.Now().UTC().Format(time.RFC3339),
		"query":             query,
		"matched_claim_ids": ids,
		"resolved":          len(ids) > 0,
	})
	fmt.Fprintln(questionLog, string(line))
}

func main() {
	// Subcommands run and exit before the server flags are parsed:
	//   init — wire a repo for capture (.mcp.json + AGENTS.md + commit hook)
	//   demo — a 30-second never-reopen walkthrough (the instant hook)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init":
			if err := runInit(os.Args[2:]); err != nil {
				log.Fatalf("lema-mcp init: %v", err)
			}
			return
		case "demo":
			if err := runDemo(os.Args[2:]); err != nil {
				log.Fatalf("lema-mcp demo: %v", err)
			}
			return
		}
	}

	adrDir := flag.String("adr-dir", "", "ADR directory (local path, or in-repo subpath with --repo). Empty = auto-discover docs/adr, doc/adr, docs/decisions, …")
	repoFlag := flag.String("repo", "", "github.com/org/repo to fetch (public; GITHUB_TOKEN for private), or a label for local --adr-dir mode")
	refFlag := flag.String("ref", "", "git ref/branch to fetch from (default: the repo's default branch); remote --repo only")
	patternFlag := flag.String("pattern", `^\d{3,4}[-_].+\.md$`, "ADR filename regex (default matches NNNN-*.md / NNN_*.md); widen for other conventions")
	openspecDir := flag.String("openspec-dir", "", "OpenSpec root (dir with specs/ and changes/) to ingest alongside ADRs; empty auto-detects ./openspec in local mode")
	captureFile := flag.String("capture-file", ".lema/decisions.jsonl", "local decision-capture store: record_decision appends here; CLOSED decisions enforce never-reopen across searches")
	flag.Parse()

	pat, err := regexp.Compile(*patternFlag)
	if err != nil {
		log.Fatalf("lema-mcp: bad --pattern %q: %v", *patternFlag, err)
	}

	if p := os.Getenv("LEMA_USAGE_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			usageLog = f
			defer usageLog.Close()
		}
	}
	if p := os.Getenv("LEMA_QUESTION_LOG"); p != "" {
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			questionLog = f
			defer questionLog.Close()
		}
	}

	// Hosted mode (ADR-0040): when LEMA_API_URL is set, search_decisions runs
	// against the hosted atom layer via POST /retrieve instead of the local
	// lexical index, and the local ADR discovery below is skipped entirely.
	if apiURL := os.Getenv("LEMA_API_URL"); apiURL != "" {
		token := os.Getenv("LEMA_API_TOKEN")
		if token == "" {
			log.Fatal("lema-mcp: LEMA_API_TOKEN is required when LEMA_API_URL is set")
		}
		src = source.NewHosted(apiURL, token, nil)
		fmt.Fprintf(os.Stderr, "lema-mcp: hosted atom search via %s (search-only)\n", apiURL)
	}

	if src == nil {
		var (
			adrs      []adr.ADR
			srcDesc   string
			localMode = true
		)
		if owner, repo, ok := parseGitHubRepo(*repoFlag); ok {
			// Remote: fetch the repo's ADRs from GitHub (no local checkout needed).
			localMode = false
			fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			var ferr error
			if adrs, repoName, ferr = fetchRemoteADRs(fctx, owner, repo, *adrDir, *refFlag, pat); ferr != nil {
				cancel()
				log.Fatalf("lema-mcp: %v", ferr)
			}
			cancel()
			srcDesc = repoName
		} else {
			// Local: parse a docs/adr-style directory on disk, auto-discovered when
			// --adr-dir is empty. Absence is tolerated — an openspec/ tree (below) may
			// supply the records instead.
			dir := *adrDir
			if dir == "" {
				dir = discoverLocalADRDir(".")
			}
			if dir != "" {
				var perr error
				if adrs, perr = adr.ParseDirMatching(dir, pat); perr != nil {
					log.Fatalf("lema-mcp: %v", perr)
				}
				srcDesc = dir
			}
			if *repoFlag != "" {
				repoName = *repoFlag
			} else if dir != "" {
				repoName = dir
			}
		}

		// OpenSpec (local only in v1): ingest an openspec/ tree alongside the ADRs,
		// numbered above the highest ADR so the two never collide in the index.
		osDir := strings.TrimSpace(*openspecDir)
		if osDir == "" && localMode {
			osDir = discoverOpenSpecDir(".")
		}
		var osRecs []adr.ADR
		if osDir != "" {
			var oerr error
			if osRecs, oerr = openspec.ParseDir(osDir, maxADRNumber(adrs)+1); oerr != nil {
				log.Fatalf("lema-mcp: openspec: %v", oerr)
			}
			if srcDesc == "" {
				srcDesc = osDir
			} else {
				srcDesc += " + " + osDir
			}
			if repoName == "local" {
				repoName = osDir
			}
		}

		records := append(adrs, osRecs...)
		if len(records) == 0 {
			log.Fatalf("lema-mcp: no decisions found (looked for ADRs in %s and an openspec/ tree); pass --adr-dir or --openspec-dir", strings.Join(adrDirCandidates, ", "))
		}
		src = source.NewLocal(records)
		fmt.Fprintf(os.Stderr, "lema-mcp: %d decisions (%d ADR, %d OpenSpec) from %s (repo %q); local lexical search, no account\n", len(records), len(adrs), len(osRecs), srcDesc, repoName)
	}

	// Decision capture (ADR-0042) is local and mode-independent: record_decision
	// writes here and search/check_decided enforce never-reopen, with or without a
	// hosted backend. A missing file is fine — the first record creates it.
	var cerr error
	if capture, cerr = source.NewCaptureStore(*captureFile); cerr != nil {
		log.Fatalf("lema-mcp: capture store: %v", cerr)
	}
	if n := capture.Len(); n > 0 {
		fmt.Fprintf(os.Stderr, "lema-mcp: %d captured decision(s) in %s\n", n, *captureFile)
	}

	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "lema-mcp", Version: "0.4.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_decisions",
		Description: "Search this repo's decisions and return the most relevant atomic claims (chosen/rejected/constraint/consequence) with their source ADR. Call this BEFORE writing or changing code to learn the constraints and what was already ruled out. Returns tight, sourced claims, not whole documents.",
	}, searchDecisions)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_decision",
		Description: "Get one decision's full body, status, and edges by its ADR number — use to drill down after search_decisions surfaces a relevant ref.",
	}, getDecision)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_decisions",
		Description: "List the architecture decisions recorded in this repo, optionally filtered by status.",
	}, listDecisions)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_decision_graph",
		Description: "Traverse typed edges (supersedes, superseded_by, depends_on, related_to) from a decision to find connected decisions.",
	}, getDecisionGraph)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "record_decision",
		Description: "Record a decision you just settled — the chosen option AND the alternatives you rejected (with why each was killed). Call this whenever you make a non-trivial choice (a library, a pattern, an architecture or policy decision). What you record becomes durable context for the next agent and is enforced: rejected and superseded options come back CLOSED so they are not re-proposed.",
	}, recordDecision)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "check_decided",
		Description: "Before proposing a direction (a library, an approach, a design), check whether it is already decided and CLOSED. Returns prior decisions that rule the option out; if anything comes back, do not re-propose it — surface the existing decision instead.",
	}, checkDecided)

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		log.Fatalf("lema-mcp: %v", err)
	}
}

// maxADRNumber returns the highest ADR number, or 0 — so OpenSpec records can be
// numbered above the ADRs without colliding in the in-memory index.
func maxADRNumber(adrs []adr.ADR) int {
	m := 0
	for _, a := range adrs {
		if a.Number > m {
			m = a.Number
		}
	}
	return m
}
