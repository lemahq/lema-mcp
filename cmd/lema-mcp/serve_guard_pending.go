package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"

	"github.com/lemahq/lema-mcp/internal/source"
)

// guardPending is one tool-call interception awaiting a human's resolution.
type guardPending struct {
	ID         string        `json:"id"`
	Tool       string        `json:"tool"`
	Closed     []source.Atom `json:"closed"`
	Resolution string        `json:"resolution,omitempty"` // "" (open) | "respect" | "override"
	Why        string        `json:"why,omitempty"`
}

// guardPendingStore holds open interceptions in memory, bridging the PreToolUse
// hook (which POSTs a tool-call and polls for the result) and the terminal UI
// (which lists open interceptions and resolves them). It is live only while the
// terminal runs its serve --http sidecar; the bare CLI hook never touches it.
// open() and get() return COPIES so no stored value escapes the lock — the handler
// can marshal them concurrently with a resolve without a data race.
type guardPendingStore struct {
	mu   sync.Mutex
	seq  int
	byID map[string]*guardPending
}

func newGuardPendingStore() *guardPendingStore {
	return &guardPendingStore{byID: map[string]*guardPending{}}
}

// add stores a new open interception and returns its id.
func (s *guardPendingStore) add(tool string, closed []source.Atom) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	id := "g" + strconv.Itoa(s.seq)
	s.byID[id] = &guardPending{ID: id, Tool: tool, Closed: closed}
	return id
}

// open returns a copy of every interception still awaiting resolution.
func (s *guardPendingStore) open() []guardPending {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []guardPending
	for _, p := range s.byID {
		if p.Resolution == "" {
			out = append(out, *p)
		}
	}
	return out
}

// resolve records a human's resolution on a pending; false if the id is unknown.
func (s *guardPendingStore) resolve(id, resolution, why string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok {
		return false
	}
	p.Resolution = resolution
	p.Why = why
	return true
}

// get returns a copy of one pending (resolved or open) by id.
func (s *guardPendingStore) get(id string) (guardPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.byID[id]
	if !ok {
		return guardPending{}, false
	}
	return *p, true
}

// guardPendings is the process-wide store of open interceptions — live only while
// the terminal runs its serve --http sidecar. Reset per-test.
var guardPendings = newGuardPendingStore()

// httpGuardPending (GET /api/guard/pending) — the terminal lists open interceptions
// to render. An empty list, never an error, when nothing is pending.
func httpGuardPending(w http.ResponseWriter, _ *http.Request) {
	writeJSONResp(w, map[string]any{"pending": guardPendings.open()})
}

// httpGuardResolve (POST /api/guard/resolve {id, resolution, why}) records the
// human's :respect / :override on a pending interception. 404 on an unknown id.
func httpGuardResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		ID         string `json:"id"`
		Resolution string `json:"resolution"`
		Why        string `json:"why"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !guardPendings.resolve(in.ID, in.Resolution, in.Why) {
		http.Error(w, "unknown pending id", http.StatusNotFound)
		return
	}
	writeJSONResp(w, map[string]any{"ok": true})
}

// httpGuardResult (GET /api/guard/result?id=) is the hook's poll: whether the human
// has resolved the interception yet, and how. 404 on an unknown id.
func httpGuardResult(w http.ResponseWriter, r *http.Request) {
	p, ok := guardPendings.get(r.URL.Query().Get("id"))
	if !ok {
		http.Error(w, "unknown pending id", http.StatusNotFound)
		return
	}
	writeJSONResp(w, map[string]any{
		"resolved":   p.Resolution != "",
		"resolution": p.Resolution,
		"why":        p.Why,
	})
}
