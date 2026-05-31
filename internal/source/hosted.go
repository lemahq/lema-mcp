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
		Type string `json:"type"`
		Ref  string `json:"ref"`
		Text string `json:"text"`
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
		atoms[i] = Atom{Type: a.Type, Ref: a.Ref, Text: a.Text}
	}
	return atoms, nil
}

func (h *Hosted) List(context.Context, string, int) ([]Summary, error) {
	return nil, errHostedSearchOnly
}
func (h *Hosted) Get(context.Context, int) (Detail, error) { return Detail{}, errHostedSearchOnly }
func (h *Hosted) Graph(context.Context, int, int) (Graph, error) {
	return Graph{}, errHostedSearchOnly
}

var _ DecisionSource = (*Hosted)(nil)
