package main

// lema settle v1 (pivot B1 — F15 as amended by adjudication A2 77c99992 and
// packaging ruling D7 a4c9d177): terminal-initiated adjudication. The
// subcommand DRAFTS the ruling — accept / reject / supersede — through the
// existing hosted write door (POST /decisions/{id}/events) and prints the
// deep link where the operator's browser click BINDS it. A programmatic
// token can never bind (ADR-0125: eventProvenance stamps every API-key
// principal actor_kind='agent'; the 0039 binding predicate is untouched), so
// the split is structural, not stylistic: the terminal moves the ruling to
// where the work is; the interactive in-app "Confirm ruling" click stays the
// only binding act. No new server surface, zero predicate change.
//
// Invoked as `lema settle ...` via the npm bin alias (the launcher forwards
// argv to this same binary) or `lema-mcp settle ...` directly.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

const settleUsage = `usage:
  lema settle accept <decision-id>... [--note <text>]
  lema settle reject <decision-id> --reason <text> [--category withdrawn|declined]
  lema settle supersede <decision-id> --by <decision-id> [--reason <text>]

<decision-id> is a full UUID, or a unique id prefix (6+ hex chars, "d_"
prefix accepted) resolved against the workspace's most recent decisions.

Every verb records a DRAFT adjudication — a programmatic credential never
binds. The printed deep link opens the decision in the browser, where
"Confirm ruling" is the binding click.`

// settleWebURLEnv overrides the web-app base used for bind deep links.
const settleWebURLEnv = "LEMA_WEB_URL"
const defaultSettleWebURL = "https://lema.sh"

// settleTimeout mirrors recordPushTimeout: interactive command, hosted API.
const settleTimeout = 30 * time.Second

// settleClient is the hosted-API surface settle needs; a struct (not the
// internal/source Hosted, which is deliberately search-only) so the command
// stays a thin door over the existing endpoints.
type settleClient struct {
	apiURL      string
	token       string
	workspaceID string
	httpClient  *http.Client
}

type settleDecision struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	CurrentStatus string `json:"current_status"`
}

func newSettleClient() (*settleClient, error) {
	apiURL, token, usedFile := resolveHostedConfig()
	if apiURL == "" || token == "" {
		return nil, fmt.Errorf("hosted credentials not configured: set %s and %s (or the credentials file)", "LEMA_API_URL", "LEMA_API_TOKEN")
	}
	if usedFile {
		// The credentials file is per-user and can point at a different org
		// than the repo you are standing in — say so, loudly, before writing.
		fmt.Fprintf(os.Stderr, "settle: using credentials file %s — verify the target below is the org you mean\n", credentialsPath())
	}
	return &settleClient{
		apiURL:      strings.TrimRight(apiURL, "/"),
		token:       token,
		workspaceID: resolveWorkspaceID(),
		httpClient:  &http.Client{Timeout: settleTimeout},
	}, nil
}

func (c *settleClient) do(method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, c.apiURL+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, out, nil
}

// apiErrorMessage digs the server's {"error": "..."} out of an error body so
// the terminal shows the server's own words, not just a status code.
func apiErrorMessage(status int, body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil && strings.TrimSpace(e.Error) != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Error, status)
	}
	return fmt.Sprintf("HTTP %d", status)
}

func (c *settleClient) getDecision(id string) (settleDecision, error) {
	status, body, err := c.do(http.MethodGet, "/decisions/"+id+"/", nil)
	if err != nil {
		return settleDecision{}, err
	}
	if status != http.StatusOK {
		return settleDecision{}, fmt.Errorf("fetch decision %s: %s", id, apiErrorMessage(status, body))
	}
	var d settleDecision
	if err := json.Unmarshal(body, &d); err != nil {
		return settleDecision{}, fmt.Errorf("decode decision %s: %w", id, err)
	}
	return d, nil
}

func (c *settleClient) listDecisions() ([]settleDecision, error) {
	if c.workspaceID == "" {
		return nil, fmt.Errorf("no workspace configured (set %s) — pass the full decision UUID instead of a prefix", workspaceIDEnv)
	}
	status, body, err := c.do(http.MethodGet, "/workspaces/"+c.workspaceID+"/decisions?limit=100", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("list decisions: %s", apiErrorMessage(status, body))
	}
	var out struct {
		Decisions []settleDecision `json:"decisions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode decision list: %w", err)
	}
	return out.Decisions, nil
}

func (c *settleClient) appendEvent(decisionID, eventType string, payload map[string]any) error {
	req := map[string]any{"type": eventType}
	if len(payload) > 0 {
		req["payload"] = payload
	}
	status, body, err := c.do(http.MethodPost, "/decisions/"+decisionID+"/events", req)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("%s: %s", eventType, apiErrorMessage(status, body))
	}
	return nil
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var hexPrefixRe = regexp.MustCompile(`^[0-9a-f]{6,}$`)

// resolveDecisionID turns operator input — a full UUID, or a 6+ hex-char id
// prefix as it appears in HANDOFF notes ("77c99992") or search locators
// ("d_b517ed", "lema:d_b517ed") — into one decision id, or fails honestly.
func (c *settleClient) resolveDecisionID(raw string) (settleDecision, error) {
	in := strings.ToLower(strings.TrimSpace(raw))
	in = strings.TrimPrefix(in, "lema:")
	in = strings.TrimPrefix(in, "d_")
	if uuidRe.MatchString(in) {
		return c.getDecision(in)
	}
	compact := strings.ReplaceAll(in, "-", "")
	if !hexPrefixRe.MatchString(compact) {
		return settleDecision{}, fmt.Errorf("%q is not a decision id: pass a full UUID or a 6+ hex-char prefix", raw)
	}
	list, err := c.listDecisions()
	if err != nil {
		return settleDecision{}, err
	}
	var matches []settleDecision
	for _, d := range list {
		if strings.HasPrefix(strings.ReplaceAll(strings.ToLower(d.ID), "-", ""), compact) {
			matches = append(matches, d)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return settleDecision{}, fmt.Errorf("no decision in the workspace's latest 100 matches prefix %q — pass the full UUID (the list read is capped, older records need the full id)", raw)
	default:
		var ids []string
		for _, m := range matches {
			ids = append(ids, fmt.Sprintf("%s (%s)", m.ID, m.Title))
		}
		return settleDecision{}, fmt.Errorf("prefix %q is ambiguous:\n  %s", raw, strings.Join(ids, "\n  "))
	}
}

// parseSettleFlags parses fs over args accepting flags BEFORE and AFTER
// positional arguments (`lema settle reject <id> --reason ...` is the
// documented shape; stdlib flag stops at the first positional). Returns the
// positional arguments in order.
func parseSettleFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func settleWebURL() string {
	if v := strings.TrimSpace(os.Getenv(settleWebURLEnv)); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultSettleWebURL
}

// printSettleResult is the one honest output shape: what was drafted, what
// it did NOT do (bind), and the single next action.
func printSettleResult(w io.Writer, verb string, d settleDecision, extra string) {
	fmt.Fprintf(w, "✓ %s drafted — %s\n", verb, d.Title)
	fmt.Fprintf(w, "  id: %s\n", d.ID)
	if extra != "" {
		fmt.Fprintf(w, "  %s\n", extra)
	}
	fmt.Fprintf(w, "  This is a DRAFT adjudication: a terminal credential never binds.\n")
	fmt.Fprintf(w, "  Bind it in the browser — open and click \"Confirm ruling\":\n")
	fmt.Fprintf(w, "    %s/decisions/%s\n", settleWebURL(), d.ID)
}

func runSettle(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("%s", settleUsage)
	}
	verb := args[0]
	rest := args[1:]

	client, err := newSettleClient()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "settle: target %s (workspace %s)\n", client.apiURL, orUnset(client.workspaceID))

	switch verb {
	case "accept":
		fs := flag.NewFlagSet("settle accept", flag.ContinueOnError)
		note := fs.String("note", "", "optional note recorded on the accept event")
		ids, err := parseSettleFlags(fs, rest)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return fmt.Errorf("usage: lema settle accept <decision-id>... [--note <text>]")
		}
		var failed []string
		for _, raw := range ids {
			d, err := client.resolveDecisionID(raw)
			if err != nil {
				fmt.Fprintf(os.Stderr, "settle: %v\n", err)
				failed = append(failed, raw)
				continue
			}
			payload := map[string]any{"via": "lema-settle"}
			if *note != "" {
				payload["note"] = *note
			}
			if err := client.appendEvent(d.ID, "accepted", payload); err != nil {
				fmt.Fprintf(os.Stderr, "settle: accept %s: %v\n", raw, err)
				failed = append(failed, raw)
				continue
			}
			printSettleResult(os.Stdout, "accept", d, statusNote(d))
		}
		if len(failed) > 0 {
			return fmt.Errorf("%d of %d accepts failed: %s", len(failed), len(ids), strings.Join(failed, ", "))
		}
		return nil

	case "reject":
		fs := flag.NewFlagSet("settle reject", flag.ContinueOnError)
		reason := fs.String("reason", "", "why the proposal is rejected (required)")
		category := fs.String("category", "declined", `"withdrawn" or "declined"`)
		pos, err := parseSettleFlags(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return fmt.Errorf("usage: lema settle reject <decision-id> --reason <text> [--category withdrawn|declined]")
		}
		if strings.TrimSpace(*reason) == "" {
			return fmt.Errorf("--reason is required: the recorded why is the product")
		}
		d, err := client.resolveDecisionID(pos[0])
		if err != nil {
			return err
		}
		if err := client.appendEvent(d.ID, "rejected", map[string]any{
			"reason_category": *category,
			"reason_body":     *reason,
			"via":             "lema-settle",
		}); err != nil {
			return err
		}
		printSettleResult(os.Stdout, "reject", d, "")
		return nil

	case "supersede":
		fs := flag.NewFlagSet("settle supersede", flag.ContinueOnError)
		by := fs.String("by", "", "the superseding decision id (required; record it first with record_decision)")
		reason := fs.String("reason", "", "optional reason recorded on the supersession")
		pos, err := parseSettleFlags(fs, rest)
		if err != nil {
			return err
		}
		if len(pos) != 1 || strings.TrimSpace(*by) == "" {
			return fmt.Errorf("usage: lema settle supersede <decision-id> --by <decision-id> [--reason <text>]")
		}
		d, err := client.resolveDecisionID(pos[0])
		if err != nil {
			return err
		}
		successor, err := client.resolveDecisionID(*by)
		if err != nil {
			return fmt.Errorf("resolve --by: %w", err)
		}
		if err := client.appendEvent(d.ID, "superseded", map[string]any{
			"superseded_by_id": successor.ID,
			"reason":           *reason,
			"via":              "lema-settle",
		}); err != nil {
			return err
		}
		printSettleResult(os.Stdout, "supersede", d,
			fmt.Sprintf("superseded by: %s (%s)", successor.ID, successor.Title))
		return nil

	default:
		return fmt.Errorf("unknown settle verb %q\n%s", verb, settleUsage)
	}
}

func statusNote(d settleDecision) string {
	if d.CurrentStatus == "accepted" {
		return "was already accepted (recall) — this draft re-confirms; the bind click is what was missing"
	}
	return "was: " + d.CurrentStatus
}

func orUnset(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}
