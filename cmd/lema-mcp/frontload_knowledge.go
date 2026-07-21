package main

// Knowledge-file frontload block (decision e886b49f): the agent seat of the
// knowledge audit. Injected alongside the recorded-decisions block so an agent
// stops trusting a dead rule BEFORE acting on it. Invariants (pinned by
// tests): zero bytes when nothing is stale — never an all-clear; at most
// knowledgeFrontloadMaxItems items, each a verbatim quote + ONE cited fact;
// the fixed abstention closer (the block never claims to verify the rest);
// fail-open everywhere. The dark gate is SERVER-side: while lema-knowledge-
// audit is off the endpoint 404s and this injects nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// knowledgeDarkTTL bounds the negative cache below: after a 404 (the server's
// dark gate) the fetch is skipped without network for this long. A flag flip
// therefore reaches hooks within one TTL — acceptable staleness for a feature
// turn-on, and it keeps a per-prompt hook from paying a guaranteed-404 round
// trip on every single prompt while the feature is dark (review finding).
const knowledgeDarkTTL = 10 * time.Minute

// errKnowledgeDark is the cached-dark skip; the runner fails open on it like
// any other fetch error.
var errKnowledgeDark = errors.New("knowledge audit dark (cached)")

// knowledgeFetcher wraps the audit GET with the marker-file negative cache.
// Fail-open throughout: an unreadable/unwritable marker just means we fetch.
type knowledgeFetcher struct {
	client      *http.Client
	apiURL      string
	token       string
	workspaceID string
	markerPath  string
	now         func() time.Time
}

func newKnowledgeFetcher(client *http.Client, apiURL, token, workspaceID string) *knowledgeFetcher {
	return &knowledgeFetcher{
		client: client, apiURL: apiURL, token: token, workspaceID: workspaceID,
		markerPath: filepath.Join(os.TempDir(), "lema-mcp-knowledge-dark-"+workspaceID),
		now:        time.Now,
	}
}

// fetch reads the workspace's audit from the hosted API. A 404 (dark) writes
// the marker so calls within the TTL skip the network; a 200 clears it.
func (f *knowledgeFetcher) fetch(ctx context.Context) ([]kfAuditFile, error) {
	if fi, err := os.Stat(f.markerPath); err == nil && f.now().Sub(fi.ModTime()) < knowledgeDarkTTL {
		return nil, errKnowledgeDark
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/workspaces/%s/knowledge-audit", strings.TrimRight(f.apiURL, "/"), f.workspaceID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	res, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close() //nolint:errcheck // read-only response body
	if res.StatusCode == http.StatusNotFound {
		_ = os.WriteFile(f.markerPath, []byte("dark"), 0o600)
		return nil, fmt.Errorf("knowledge audit: %s", res.Status)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("knowledge audit: %s", res.Status)
	}
	var out struct {
		Files []kfAuditFile `json:"files"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	_ = os.Remove(f.markerPath)
	return out.Files, nil
}

type kfAuditWire struct {
	Artifact string `json:"artifact"`
	State    string `json:"state"`
	Detail   string `json:"detail"`
}

type kfAuditAnchor struct {
	Title  string `json:"title"`
	Status string `json:"status"`
}

type kfAuditRule struct {
	Text     string         `json:"text"`
	Headline string         `json:"headline"`
	Wire     *kfAuditWire   `json:"wire"`
	Anchor   *kfAuditAnchor `json:"anchor"`
}

type kfAuditFile struct {
	Path  string        `json:"path"`
	AsOf  string        `json:"as_of"`
	Rules []kfAuditRule `json:"rules"`
}

const (
	knowledgeFrontloadMaxItems   = 3
	knowledgeFrontloadQuoteRunes = 120
)

// renderKnowledgeFrontload builds the stdout block, or "" when no rule is
// stale.
func renderKnowledgeFrontload(files []kfAuditFile) string {
	type item struct {
		path, quote, fact string
	}
	var (
		items  []item
		total  int
		latest string // RFC3339 compares lexically
	)
	for _, f := range files {
		if f.AsOf > latest {
			latest = f.AsOf
		}
		for _, r := range f.Rules {
			if r.Headline != "stale" {
				continue
			}
			total++
			if len(items) >= knowledgeFrontloadMaxItems {
				continue
			}
			items = append(items, item{path: f.Path, quote: truncateQuote(r.Text), fact: staleFact(r)})
		}
	}
	if total == 0 {
		return "" // clean is ZERO bytes — never an all-clear
	}

	var b strings.Builder
	b.WriteString("Knowledge-file audit from lema (as of last check ")
	b.WriteString(latest)
	b.WriteString("):\n")
	fmt.Fprintf(&b, "%d rule(s) in your knowledge files no longer match the record or the repo — verify before relying on them:\n", total)
	for i, it := range items {
		fmt.Fprintf(&b, "[%d] %s: %q — %s\n", i+1, it.path, it.quote, it.fact)
	}
	b.WriteString("This is not a verification of the files' other rules: most carry no citation or path lema can check. When in doubt, ask the record (search_decisions / check_decided).\n")
	return b.String()
}

// staleFact states the ONE cited fact behind a stale rule — the engine's
// observation for a tripped wire, or the cited decision's status. Facts only:
// no verdict adjectives, no advice beyond re-verification.
func staleFact(r kfAuditRule) string {
	if r.Wire != nil && r.Wire.State == "tripped" {
		if d := strings.TrimSpace(r.Wire.Detail); d != "" {
			return fmt.Sprintf("the artifact %s changed: %s", r.Wire.Artifact, d)
		}
		return fmt.Sprintf("the artifact %s changed", r.Wire.Artifact)
	}
	if r.Anchor != nil {
		return fmt.Sprintf("the cited decision %q is %s", r.Anchor.Title, r.Anchor.Status)
	}
	return "flagged by the last check"
}

func truncateQuote(s string) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= knowledgeFrontloadQuoteRunes {
		return s
	}
	return string(runes[:knowledgeFrontloadQuoteRunes]) + "…"
}
