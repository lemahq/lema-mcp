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

// errHostedSearchOnly is returned by the non-search tools in hosted mode: the
// MVP (ADR-0040) wires only search_decisions to the hosted atom layer; list/get/
// graph stay local-only for now.
var errHostedSearchOnly = errors.New("hosted mode supports search only in this MVP; run lema-mcp without LEMA_API_URL for list/get/graph")

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

func (h *Hosted) List(context.Context, string, int) ([]Summary, error) {
	return nil, errHostedSearchOnly
}
func (h *Hosted) Get(context.Context, int) (Detail, error) { return Detail{}, errHostedSearchOnly }
func (h *Hosted) Graph(context.Context, int, int) (Graph, error) {
	return Graph{}, errHostedSearchOnly
}

var _ DecisionSource = (*Hosted)(nil)
