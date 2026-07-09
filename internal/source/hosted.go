package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// errHostedSearchOnly is returned by the non-search tools in hosted mode: the
// MVP (ADR-0040) wires only search_decisions to the hosted atom layer; list/get/
// graph stay local-only for now.
var errHostedSearchOnly = errors.New("hosted mode supports search only in this MVP; run lema-mcp without LEMA_API_URL for list/get/graph")

// ErrHostedQuotaReached is returned by CheckApproach when the authed endpoint
// answers 429 — the plan-aware daily query quota (ADR-0103). It is the authed
// sibling of ErrPublicRateLimited: the caller converts it into an honest "limit
// reached" message rather than a raw tool error, so a paying user hitting their
// quota gets the same graceful degrade the tokenless leg already gives.
var ErrHostedQuotaReached = errors.New("hosted query quota reached")

// Hosted is a DecisionSource backed by a lema deployment's POST /retrieve
// (ADR-0040): it sends the query with a bearer token and maps the returned
// atoms. Only Search is implemented; the other tools report the limitation.
type Hosted struct {
	baseURL string
	token   string
	hc      *http.Client
}

// NewHosted builds a hosted source. baseURL is the lema-api root; token is the
// agent bearer token. A nil client gets a sensible default with a timeout.
func NewHosted(baseURL, token string, hc *http.Client) *Hosted {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Hosted{baseURL: strings.TrimRight(baseURL, "/"), token: token, hc: hc}
}

type hostedRetrieveReq struct {
	Query string `json:"query"`
	K     int    `json:"k"`
}

type hostedRetrieveResp struct {
	Atoms []struct {
		Type    string `json:"type"`
		Ref     string `json:"ref"`
		Text    string `json:"text"`
		Locator string `json:"locator"`
	} `json:"atoms"`
}

// Search posts the query to the hosted /retrieve and maps the atoms into the
// MCP's consumption shape.
func (h *Hosted) Search(ctx context.Context, query string, k int) ([]Atom, error) {
	body, err := json.Marshal(hostedRetrieveReq{Query: query, K: k})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/retrieve", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)

	resp, err := h.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hosted retrieve: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hosted retrieve: status %d", resp.StatusCode)
	}
	var out hostedRetrieveResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("hosted retrieve decode: %w", err)
	}
	atoms := make([]Atom, len(out.Atoms))
	for i, a := range out.Atoms {
		atoms[i] = Atom{Type: a.Type, Ref: a.Ref, Text: a.Text, Locator: a.Locator}
	}
	return atoms, nil
}

// AskSource is one cited claim in an Ask answer: the typed claim, its source
// ref, and (additively) the followable locator/url so the agent can open the
// artifact behind a [n] citation. Mirrors the api askSource wire shape.
type AskSource struct {
	N         int    `json:"n"`
	Ref       string `json:"ref"`
	Type      string `json:"type"`
	Text      string `json:"text"`
	Locator   string `json:"locator,omitempty"`
	URL       string `json:"url,omitempty"`
	Status    string `json:"status,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	// RejectedAlternatives + Relevance are served by the api (ask.go askSource)
	// and were dropped at decode before WP1. Relevance is a *float64 cosine
	// similarity in [0,1] — the same axis the 0.38 floor compares — NOT a
	// confidence; nil when the atom has no dense distance (fts-only).
	RejectedAlternatives []string `json:"rejected_alternatives,omitempty"`
	Relevance            *float64 `json:"relevance,omitempty"`
	// DecisionURL is the citation's stable public permalink ({web}/d/{id},
	// week-1 spread floor) — present only on public-corpus answers
	// (/ask-public); the authed /ask never serves a public URL.
	DecisionURL string `json:"decision_url,omitempty"`
}

// AskUsage is the token meter the hosted /ask returns: the saving side (claims
// served vs the cited source bodies) plus, for honesty, the synthesis cost the
// answer itself spent.
type AskUsage struct {
	AtomsTokens      int     `json:"atoms_tokens"`
	SourceTokens     int     `json:"source_tokens"`
	TokensSaved      int     `json:"tokens_saved"`
	CompressionRatio float64 `json:"compression_ratio"`
	SynthesisTokens  int     `json:"synthesis_tokens"`
}

// AskResult is a synthesized, cited answer from the hosted /ask: the prose
// answer (with inline [n] citations), the cited sources those [n] point at, and
// the token meter. This is the join the local DB-less binary cannot produce — it
// needs the hosted retrieval + synthesis — so Ask exists only on Hosted.
type AskResult struct {
	Scope   string      `json:"scope"`
	Answer  string      `json:"answer"`
	Sources []AskSource `json:"sources"`
	Usage   AskUsage    `json:"usage"`
}

type hostedAskReq struct {
	Query        string   `json:"query"`
	WorkspaceIDs []string `json:"workspace_ids,omitempty"`
}

type hostedAskResp struct {
	Scope   string      `json:"scope"`
	Answer  string      `json:"answer"`
	Sources []AskSource `json:"sources"`
	Usage   struct {
		AtomsTokens      int     `json:"atoms_tokens"`
		SourceTokens     int     `json:"source_tokens"`
		TokensSaved      int     `json:"tokens_saved"`
		CompressionRatio float64 `json:"compression_ratio"`
	} `json:"usage"`
	SynthesisTokensIn  int `json:"synthesis_tokens_in"`
	SynthesisTokensOut int `json:"synthesis_tokens_out"`
}

// Ask posts the query (and optional workspace focus) to the hosted POST /ask and
// returns the synthesized, cited answer. Hosted-only by design (ADR-0059 shape
// A): the answer requires the hosted hybrid retrieval + Vertex synthesis the
// local binary deliberately does not carry, so the local DB-less/LLM-free wedge
// is untouched.
func (h *Hosted) Ask(ctx context.Context, query string, workspaceIDs []string) (AskResult, error) {
	body, err := json.Marshal(hostedAskReq{Query: query, WorkspaceIDs: workspaceIDs})
	if err != nil {
		return AskResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/ask", bytes.NewReader(body))
	if err != nil {
		return AskResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)

	resp, err := h.hc.Do(req)
	if err != nil {
		return AskResult{}, fmt.Errorf("hosted ask: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return AskResult{}, fmt.Errorf("hosted ask: status %d", resp.StatusCode)
	}
	var out hostedAskResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return AskResult{}, fmt.Errorf("hosted ask decode: %w", err)
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
			// Fold the two synthesis legs into one cost number for the agent.
			SynthesisTokens: out.SynthesisTokensIn + out.SynthesisTokensOut,
		},
	}, nil
}

// hostedCheckApproachReq is the authed Fusion query (#293): the approach to check
// against the caller's own corpus, optionally focused to specific workspaces. The
// workspace_ids key is omitted when empty so the server resolves the caller's full
// scope rather than an empty list.
type hostedCheckApproachReq struct {
	Approach     string   `json:"approach"`
	WorkspaceIDs []string `json:"workspace_ids,omitempty"`
}

// CheckApproach POSTs {approach, workspace_ids} to the authed POST /check-approach
// and returns the fused verdict (ruled_out with the cited why-not, or the honest
// no_recorded_ruling with the recall-WHY reasoning) over the CALLER'S OWN corpus
// (#293, ADR-0124). It is the authed sibling of Public.Fuse: the same fuseWire
// response shape, but with a bearer token and an own-corpus query instead of a public
// slug — so the MCP tool maps an own-repo verdict exactly like a commons one. Hosted-
// only by design: answering over the user's recorded decisions needs the hosted
// retrieval + Vertex synthesis the local DB-less binary deliberately does not carry.
func (h *Hosted) CheckApproach(ctx context.Context, approach string, workspaceIDs []string) (FuseResult, error) {
	body, err := json.Marshal(hostedCheckApproachReq{Approach: approach, WorkspaceIDs: workspaceIDs})
	if err != nil {
		return FuseResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/check-approach", bytes.NewReader(body))
	if err != nil {
		return FuseResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)

	resp, err := h.hc.Do(req)
	if err != nil {
		return FuseResult{}, fmt.Errorf("hosted check-approach: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return FuseResult{}, ErrHostedQuotaReached
	}
	if resp.StatusCode != http.StatusOK {
		return FuseResult{}, fmt.Errorf("hosted check-approach: status %d", resp.StatusCode)
	}
	var out fuseWire // same wire shape as POST /fuse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return FuseResult{}, fmt.Errorf("hosted check-approach decode: %w", err)
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

func (h *Hosted) List(context.Context, string, int) ([]Summary, error) {
	return nil, errHostedSearchOnly
}
func (h *Hosted) Get(context.Context, int) (Detail, error) { return Detail{}, errHostedSearchOnly }
func (h *Hosted) Graph(context.Context, int, int) (Graph, error) {
	return Graph{}, errHostedSearchOnly
}

var _ DecisionSource = (*Hosted)(nil)

// ClosedFetcher is the context-aware, fallible sibling of ClosedSource: the
// capability a network-backed DecisionSource exposes when it can fetch the
// org's CLOSED no-go atoms from the hosted graph (GET /closed-atoms,
// build-plan D.1). It is a separate interface — not a ClosedSource
// implementation — because a network fetch can fail, and check_decided must
// surface that failure rather than silently degrade to local-only checking
// (the exact bug the hosted leg exists to fix).
type ClosedFetcher interface {
	// FetchClosedAtoms returns the org's CLOSED no-go set. When workspaceIDs is
	// non-empty the set is scoped to those workspaces (so a check_decided run in
	// one repo never trips on another repo's rejected option); empty means every
	// workspace the caller can see.
	FetchClosedAtoms(ctx context.Context, workspaceIDs []string) ([]Atom, error)
}

type hostedClosedResp struct {
	Atoms []struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Text       string `json:"text"`
		Ref        string `json:"ref"`
		Locator    string `json:"locator"`
		Closed     bool   `json:"closed"`
		ClosedNote string `json:"closed_note"`
		MatchKey   string `json:"match_key"`
	} `json:"atoms"`
	Truncated bool `json:"truncated"`
}

// FetchClosedAtoms GETs the hosted never-reopen feed: the org's rejected
// alternatives from accepted, non-superseded decisions (ADR-0053 semantics
// served server-side). The served match_key lands on Atom.MatchKey so the
// client-side weighted matcher treats hosted closures exactly like locally
// captured ones.
func (h *Hosted) FetchClosedAtoms(ctx context.Context, workspaceIDs []string) ([]Atom, error) {
	u := h.baseURL + "/closed-atoms"
	if len(workspaceIDs) > 0 {
		q := url.Values{}
		for _, id := range workspaceIDs {
			q.Add("workspace_ids", id)
		}
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)

	resp, err := h.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hosted closed-atoms: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hosted closed-atoms: status %d", resp.StatusCode)
	}
	var out hostedClosedResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("hosted closed-atoms decode: %w", err)
	}
	atoms := make([]Atom, len(out.Atoms))
	for i, a := range out.Atoms {
		atoms[i] = Atom{
			ID:         a.ID,
			Type:       a.Type,
			Text:       a.Text,
			Ref:        a.Ref,
			Locator:    a.Locator,
			Closed:     a.Closed,
			ClosedNote: a.ClosedNote,
			MatchKey:   a.MatchKey,
		}
	}
	return atoms, nil
}

var _ ClosedFetcher = (*Hosted)(nil)
