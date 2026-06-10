package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lemahq/lema-mcp/internal/source"
)

// Wire contract with POST /workspaces/{id}/import-decisions (schema_version 1).
// The records are source.DecisionRecord verbatim — the local schema IS the
// wire schema; the hosted side validates and maps (it never trusts these).
type pushRequest struct {
	SchemaVersion int                     `json:"schema_version"`
	DryRun        bool                    `json:"dry_run,omitempty"`
	Records       []source.DecisionRecord `json:"records"`
}

// pushResult is the per-record outcome the server reports: which local id it
// is, what happened (created|updated|skipped|failed), and why.
type pushResult struct {
	LocalID    string   `json:"local_id"`
	Title      string   `json:"title"`
	Status     string   `json:"status"`
	Reason     string   `json:"reason,omitempty"`
	DecisionID *string  `json:"decision_id,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// pushResponse is one batch's response from the server; pushRecords also uses
// it as the aggregate across batches (counters summed, results appended).
type pushResponse struct {
	Created int          `json:"created"`
	Updated int          `json:"updated"`
	Skipped int          `json:"skipped"`
	Failed  int          `json:"failed"`
	DryRun  bool         `json:"dry_run,omitempty"`
	Results []pushResult `json:"results"`
}

// pushMaxBatch is the server's hard batch cap; it doubles as the default when
// the caller passes a non-positive batch size.
const pushMaxBatch = 500

// pushRecords sends records to POST {apiURL}/workspaces/{workspace}/import-decisions
// in batches of batchSize and aggregates the responses. The server is
// idempotent on record id, so a retried or overlapping push is safe. On a
// failed batch it returns what aggregated so far plus an error carrying both
// the HTTP status and the server's error message — remaining batches are not
// attempted (the failure almost certainly repeats: auth, membership, schema).
func pushRecords(ctx context.Context, apiURL, token, workspace string, records []source.DecisionRecord, batchSize int, dryRun bool) (pushResponse, error) {
	if batchSize <= 0 || batchSize > pushMaxBatch {
		batchSize = pushMaxBatch
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	url := strings.TrimRight(apiURL, "/") + "/workspaces/" + workspace + "/import-decisions"

	var agg pushResponse
	// What we requested, not what the server echoed: the aggregate's DryRun
	// reflects this client's intent even when zero batches complete.
	agg.DryRun = dryRun
	for start := 0; start < len(records); start += batchSize {
		end := min(start+batchSize, len(records))
		resp, err := pushBatch(ctx, hc, url, token, pushRequest{
			SchemaVersion: 1,
			DryRun:        dryRun,
			Records:       records[start:end],
		})
		if err != nil {
			return agg, err
		}
		agg.Created += resp.Created
		agg.Updated += resp.Updated
		agg.Skipped += resp.Skipped
		agg.Failed += resp.Failed
		agg.Results = append(agg.Results, resp.Results...)
	}
	return agg, nil
}

// pushBatch posts one batch and decodes the response. Non-200 replies are
// turned into an error that carries the status code and, when the body is the
// API's {"error": "..."} shape, the server's message — the user must see WHY
// a push was refused, not just that it was.
func pushBatch(ctx context.Context, hc *http.Client, url, token string, body pushRequest) (pushResponse, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return pushResponse{}, fmt.Errorf("push encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return pushResponse{}, fmt.Errorf("push request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := hc.Do(req)
	if err != nil {
		return pushResponse{}, fmt.Errorf("push: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var apiErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error != "" {
			return pushResponse{}, fmt.Errorf("push: status %d: %s", resp.StatusCode, apiErr.Error)
		}
		return pushResponse{}, fmt.Errorf("push: status %d", resp.StatusCode)
	}

	var out pushResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return pushResponse{}, fmt.Errorf("push decode: %w", err)
	}
	return out, nil
}

// pushConfig persists non-secret defaults next to the store (.lema/push.json)
// so the second run is just `lema-mcp push`. The token is env-only by design —
// it must never be written here.
type pushConfig struct {
	Workspace string `json:"workspace,omitempty"`
	APIURL    string `json:"api_url,omitempty"`
}

// pushConfigFile is the config filename inside the .lema directory.
const pushConfigFile = "push.json"

// loadPushConfig reads dir/push.json. A missing file is not an error — it
// yields the zero config (first run, nothing saved yet).
func loadPushConfig(dir string) (pushConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dir, pushConfigFile))
	if err != nil {
		if os.IsNotExist(err) {
			return pushConfig{}, nil
		}
		return pushConfig{}, fmt.Errorf("read push config: %w", err)
	}
	var cfg pushConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return pushConfig{}, fmt.Errorf("parse push config: %w", err)
	}
	return cfg, nil
}

// savePushConfig writes dir/push.json, creating dir if needed. Indented so a
// human can read (and audit) what the second run will default to.
func savePushConfig(dir string, cfg pushConfig) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode push config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, pushConfigFile), append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("write push config: %w", err)
	}
	return nil
}

// runPush is the `lema-mcp push` entry point: read the capture store's
// reduced view, POST it to the hosted workspace, report per-record outcomes.
// Idempotent by server contract — safe to run anytime, by anyone, including
// against a teammate's identical committed .lema file.
func runPush(args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	captureFile := fs.String("capture-file", ".lema/decisions.jsonl", "local decision-capture store to push")
	workspaceFlag := fs.String("workspace", "", "hosted workspace id to push into (default: remembered from the last push)")
	apiURLFlag := fs.String("api-url", "", "hosted API base URL (default: remembered from the last push, else LEMA_API_URL)")
	dryRun := fs.Bool("dry-run", false, "validate against the server and report outcomes without writing anything")
	batchSize := fs.Int("batch-size", 200, "records per request (1–500)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// User-facing validation: a typed-out batch size outside the server cap is
	// a mistake worth naming, not something to silently clamp (pushRecords
	// keeps the clamp only as a backstop for other callers).
	if *batchSize < 1 || *batchSize > pushMaxBatch {
		return fmt.Errorf("--batch-size %d is out of range: must be between 1 and %d", *batchSize, pushMaxBatch)
	}

	// Precedence: flag > config > env (env for api-url only). The token is
	// env-only by design — it is never read from or written to disk here.
	cfg, err := loadPushConfig(filepath.Dir(*captureFile))
	if err != nil {
		fmt.Fprintf(os.Stderr, "lema-mcp push: ignoring unreadable config: %v\n", err)
		cfg = pushConfig{}
	}
	workspace := *workspaceFlag
	if workspace == "" {
		workspace = cfg.Workspace
	}
	apiURL := *apiURLFlag
	if apiURL == "" {
		apiURL = cfg.APIURL
	}
	if apiURL == "" {
		apiURL = os.Getenv("LEMA_API_URL")
	}
	token := os.Getenv("LEMA_API_TOKEN")

	// Dry-run still needs all three: it talks to the server.
	if workspace == "" {
		return fmt.Errorf("no workspace: pass --workspace <id> (copy it from your workspace URL in the lema web app); it is remembered for next time")
	}
	if apiURL == "" {
		return fmt.Errorf("no API URL: pass --api-url or set LEMA_API_URL")
	}
	if token == "" {
		return fmt.Errorf("LEMA_API_TOKEN is not set: create an API key in the lema web dashboard and export it")
	}

	store, err := source.NewCaptureStore(*captureFile)
	if err != nil {
		return fmt.Errorf("open capture store: %w", err)
	}
	records := store.Records()
	if len(records) == 0 {
		fmt.Printf("nothing to push: %s is empty\n", *captureFile)
		return nil
	}

	resp, pushErr := pushRecords(context.Background(), apiURL, token, workspace, records, *batchSize, *dryRun)

	// Report whatever aggregated — on a mid-push failure the completed batches'
	// outcomes are real (the server applied them) and the user must see them.
	if pushErr == nil || resp.Created+resp.Updated+resp.Skipped+resp.Failed > 0 || len(resp.Results) > 0 {
		verb := "pushed"
		if *dryRun {
			verb = "would push"
		}
		total := resp.Created + resp.Updated + resp.Skipped + resp.Failed
		fmt.Printf("%s %d record(s) to workspace %s: %d created, %d updated, %d skipped (unchanged), %d failed\n",
			verb, total, workspace, resp.Created, resp.Updated, resp.Skipped, resp.Failed)
		for _, r := range resp.Results {
			for _, w := range r.Warnings {
				fmt.Printf("  warn %s (%s): %s\n", r.LocalID, r.Title, w)
			}
			if r.Status == "failed" {
				fmt.Printf("  FAIL %s (%s): %s\n", r.LocalID, r.Title, r.Reason)
			}
		}
	}
	if pushErr != nil {
		return pushErr
	}

	// Remember the working pair for the next bare `lema-mcp push` — only after
	// a real push where at least one record was accepted. A fully-failed push
	// has not proven the workspace/url pair works, so we must not persist it:
	// doing so would silently cache a bad config and break every subsequent bare
	// `lema-mcp push`. Dry runs also leave no trace by design.
	if !*dryRun && resp.Created+resp.Updated+resp.Skipped > 0 {
		if err := savePushConfig(filepath.Dir(*captureFile), pushConfig{Workspace: workspace, APIURL: apiURL}); err != nil {
			fmt.Fprintf(os.Stderr, "lema-mcp push: could not save config for next time: %v\n", err)
		}
	}

	if resp.Failed > 0 {
		return fmt.Errorf("%d record(s) failed — see FAIL lines above", resp.Failed)
	}
	return nil
}
