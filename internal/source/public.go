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

// FuseSource is one cited rejection behind a /fuse ruled_out verdict. N is the
// 1-based citation index the why_not [n] markers point at.
type FuseSource struct {
	N             int      `json:"n"`
	Ref           string   `json:"ref"`
	Type          string   `json:"type"`
	Text          string   `json:"text"`
	URL           string   `json:"url,omitempty"`
	BindingCosine *float64 `json:"binding_cosine,omitempty"`
	// DecisionURL is the citation's stable public permalink ({web}/d/{id},
	// week-1 spread floor) — served by the public /fuse path so an agent can
	// paste the recorded why into a PR thread. Absent on authed own-corpus
	// verdicts (a private decision never gets a public URL).
	DecisionURL string `json:"decision_url,omitempty"`
}

// FuseHow is the repo-level HOW pointer: the project's canonical docs URL plus a
// topic hint. No vendor identifier — a plain URL (ADR-0099). The two-stream fields
// (ADR-0120) ride additively and are present only when the server has LEMA_FUSE_HOW on.
type FuseHow struct {
	DocHome string `json:"doc_home,omitempty"`
	Topic   string `json:"topic,omitempty"`
	// SanctionedAlternative is the code-aligned name for the what-instead (the topic on
	// the ruled_out path); Grounding is its provenance: corpus_chosen | doc_extract |
	// pointer | none.
	SanctionedAlternative string `json:"sanctioned_alternative,omitempty"`
	Grounding             string `json:"grounding,omitempty"`
	// Guidance / Citation / FetchedAt are the Phase-2 deref (ADR-0120): a verbatim snippet
	// from the project's own docs, the real fetched section it came from, and the instant
	// it was fetched (a FACT about when the snippet was read, NOT a liveness claim — it is
	// frozen in the verdict cache, so on a cache hit it can be much older than the response).
	// Present only on a deref hit; absent on a pointer/none degrade.
	Guidance  string           `json:"guidance,omitempty"`
	Citation  *FuseHowCitation `json:"citation,omitempty"`
	FetchedAt string           `json:"fetched_at,omitempty"`
}

// FuseHowCitation is the deref's real section pointer: a fetched-200 URL (host ∈ the
// project's doc hosts) and its human-facing section label (ADR-0120).
type FuseHowCitation struct {
	URL     string `json:"url,omitempty"`
	Section string `json:"section,omitempty"`
}

// FuseResult is the Fusion verdict: ruled_out (with the cited why-not + how) or
// no_recorded_ruling (honest abstain). Decoded from POST /fuse.
type FuseResult struct {
	Repo     string `json:"repo"`
	Approach string `json:"approach"`
	Verdict  string `json:"verdict"`
	WhyNot   string `json:"why_not"`
	// Why is the ADR-0121 recall-WHY synthesis: recorded reasoning the backend
	// serves on a no_recorded_ruling when retrieval grounded something but no
	// ruling fired (fuse.go writeFuseRecallWhy). It is reasoning, never a ruling.
	Why     string       `json:"why,omitempty"`
	Sources []FuseSource `json:"sources"`
	How     FuseHow      `json:"how"`
	Note    string       `json:"note"`
	Usage   AskUsage     `json:"-"`
}

// fuseWire decodes the raw /fuse response, folding the two synthesis legs into
// one AskUsage (the same meter shape /ask-public serves).
type fuseWire struct {
	FuseResult
	Usage struct {
		AtomsTokens      int     `json:"atoms_tokens"`
		SourceTokens     int     `json:"source_tokens"`
		TokensSaved      int     `json:"tokens_saved"`
		CompressionRatio float64 `json:"compression_ratio"`
	} `json:"usage"`
	SynthesisTokensIn  int `json:"synthesis_tokens_in"`
	SynthesisTokensOut int `json:"synthesis_tokens_out"`
}

type fuseReq struct {
	Slug     string `json:"slug"`
	Approach string `json:"approach"`
}

// Fuse POSTs {slug, approach} to the no-auth POST /fuse and returns the fused
// verdict (why-not + how, or honest abstain). 404 -> ErrPublicGraphNotLoaded,
// 429 -> ErrPublicRateLimited (same honest degradation as PublicAsk).
func (p *Public) Fuse(ctx context.Context, slug, approach string) (FuseResult, error) {
	body, err := json.Marshal(fuseReq{Slug: slug, Approach: approach})
	if err != nil {
		return FuseResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/fuse", bytes.NewReader(body))
	if err != nil {
		return FuseResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return FuseResult{}, fmt.Errorf("fuse: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return FuseResult{}, ErrPublicGraphNotLoaded
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return FuseResult{}, ErrPublicRateLimited
	}
	if resp.StatusCode != http.StatusOK {
		return FuseResult{}, fmt.Errorf("fuse: status %d", resp.StatusCode)
	}
	var out fuseWire
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return FuseResult{}, fmt.Errorf("fuse decode: %w", err)
	}
	res := out.FuseResult
	res.Usage = AskUsage{
		AtomsTokens:      out.Usage.AtomsTokens,
		SourceTokens:     out.Usage.SourceTokens,
		TokensSaved:      out.Usage.TokensSaved,
		CompressionRatio: out.Usage.CompressionRatio,
		SynthesisTokens:  out.SynthesisTokensIn + out.SynthesisTokensOut,
	}
	return res, nil
}
