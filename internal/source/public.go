package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Public is the tokenless client for Lema's PUBLIC demo graphs (React/k8s/Rust)
// served at the no-auth POST /ask-public. Unlike Hosted it sends NO bearer token
// and addresses a workspace by its public slug — the /ask-public perimeter is
// gated on the workspace's public_demo flag, not on auth. Only Ask is needed:
// the public surface is read-only synthesis.
type Public struct {
	baseURL string
	hc      *http.Client
}

// NewPublic builds a public client. baseURL is the public lema-api root; a nil
// client gets a sensible default with a timeout.
func NewPublic(baseURL string, hc *http.Client) *Public {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Public{baseURL: strings.TrimRight(baseURL, "/"), hc: hc}
}

// ErrPublicGraphNotLoaded is returned when /ask-public 404s for a slug — the
// repo's graph is not seeded/live yet. The caller degrades honestly rather than
// implying the repo answered.
var ErrPublicGraphNotLoaded = errors.New("public graph not loaded")

// ErrPublicRateLimited is returned when /ask-public returns 429 — the per-IP
// rate limit or the per-day demo ceiling. The caller converts it into an honest
// "free public limit reached — connect your repo for more" message, not an error.
var ErrPublicRateLimited = errors.New("public rate limited")

type publicAskReq struct {
	Slug  string `json:"slug"`
	Query string `json:"query"`
}

// PublicAsk POSTs {slug, query} to the no-auth POST /ask-public and returns the
// synthesized, cited answer. It mirrors Hosted.Ask with exactly two diffs: no
// Authorization header, and a {slug,query} body. The response is the same
// askResponse shape /ask returns (ask_public.go reuses it), so it decodes into
// hostedAskResp. A 404 -> ErrPublicGraphNotLoaded; other non-200s -> error.
func (p *Public) PublicAsk(ctx context.Context, slug, query string) (AskResult, error) {
	body, err := json.Marshal(publicAskReq{Slug: slug, Query: query})
	if err != nil {
		return AskResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/ask-public", bytes.NewReader(body))
	if err != nil {
		return AskResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Deliberately NO Authorization header — public_demo gates the perimeter.

	resp, err := p.hc.Do(req)
	if err != nil {
		return AskResult{}, fmt.Errorf("public ask: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return AskResult{}, ErrPublicGraphNotLoaded
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return AskResult{}, ErrPublicRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		return AskResult{}, fmt.Errorf("public ask: status %d", resp.StatusCode)
	}
	var out hostedAskResp // same wire shape as /ask
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return AskResult{}, fmt.Errorf("public ask decode: %w", err)
	}
	return AskResult{
		Scope:   out.Scope,
		Answer:  out.Answer,
		Sources: out.Sources,
		Usage: AskUsage{
			AtomsTokens:      out.Usage.AtomsTokens,
			SourceTokens:     out.Usage.SourceTokens,
			TokensSaved:      out.Usage.TokensSaved,
			CompressionRatio: out.Usage.CompressionRatio,
			SynthesisTokens:  out.SynthesisTokensIn + out.SynthesisTokensOut,
		},
	}, nil
}

// SettledDecision is one governing decision surfaced by /settled.
type SettledDecision struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

// SettledResult is the typed public verdict: state + the recorded reasoning.
type SettledResult struct {
	Repo      string            `json:"repo"`
	Topic     string            `json:"topic"`
	Settled   string            `json:"settled"`
	Decisions []SettledDecision `json:"decisions"`
	Note      string            `json:"note"`
}

type settledReq struct {
	Slug  string `json:"slug"`
	Topic string `json:"topic"`
}

// Settled POSTs {slug, topic} to the no-auth POST /settled and returns the typed
// "is this already decided, and why?" result. 404 -> ErrPublicGraphNotLoaded,
// 429 -> ErrPublicRateLimited (same honest degradation as PublicAsk).
func (p *Public) Settled(ctx context.Context, slug, topic string) (SettledResult, error) {
	body, err := json.Marshal(settledReq{Slug: slug, Topic: topic})
	if err != nil {
		return SettledResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/settled", bytes.NewReader(body))
	if err != nil {
		return SettledResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return SettledResult{}, fmt.Errorf("settled: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return SettledResult{}, ErrPublicGraphNotLoaded
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return SettledResult{}, ErrPublicRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		return SettledResult{}, fmt.Errorf("settled: status %d", resp.StatusCode)
	}
	var out SettledResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return SettledResult{}, fmt.Errorf("settled decode: %w", err)
	}
	return out, nil
}
