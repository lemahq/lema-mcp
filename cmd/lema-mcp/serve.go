package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lemahq/lema-mcp/internal/docs"
	"github.com/lemahq/lema-mcp/internal/source"
)

// serveHTTP exposes the local engine — the SAME source.DecisionSource +
// CaptureStore the stdio MCP tools use — over localhost as a small REST/JSON API
// for the lema Workspaces GUI (ADR-0043/0044). It is the human-facing transport
// alongside the agent-facing stdio server; the JSON mirrors the MCP tool outputs
// so one set of UI components renders against local `serve --http` or hosted
// `apps/api`. Bound to 127.0.0.1, token-guarded, CORS-scoped to the GUI origin.
func serveHTTP(port int) error {
	token := httpToken()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(w, map[string]any{"ok": true, "repo": repoName})
	})
	mux.HandleFunc("/api/search", httpSearch)                // GET ?q=&k=&max_tokens=
	mux.HandleFunc("/api/decisions", httpList)               // GET ?status=&limit=
	mux.HandleFunc("/api/decision", httpGet)                 // GET ?number=
	mux.HandleFunc("/api/graph", httpGraph)                  // GET ?number=&depth=
	mux.HandleFunc("/api/check", httpCheck)                  // GET ?topic=
	mux.HandleFunc("/api/decided", httpDecided)              // GET — all currently-CLOSED captures (enforcement feed)
	mux.HandleFunc("/api/record", httpRecord)                // POST DecisionRecord
	mux.HandleFunc("/api/init", httpInit)                    // POST — register lema-mcp in the repo
	mux.HandleFunc("/api/plugins", httpPlugins)              // GET — Plugins panel snapshot (ADR-0043/0044)
	mux.HandleFunc("/api/plugins/toggle", httpPluginsToggle) // POST {kind,name,enabled} — toggle mcp server or lema hook
	mux.HandleFunc("/api/sessions", httpSessions)            // GET ?limit — session list (ADR-0046/0049)
	mux.HandleFunc("/api/session", httpSession)              // GET ?id=&prompts= — one session in full
	mux.HandleFunc("/api/docs", httpDocs)                    // GET — project-docs listing (ADR-0055)
	mux.HandleFunc("/api/doc", httpDoc)                      // GET ?path=&section=&max_tokens= — one doc or section

	handler := withCORS(withToken(token, mux))
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	// Bind the listener BEFORE announcing the URL/token so the log line (and any
	// caller racing on it) is truthful: once these lines print, the socket is
	// accepting. The Tauri shell does not trust this log anyway — it polls
	// /healthz for actual readiness — but a bound-then-logged order means the
	// stderr line never lies about availability.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	// Self-terminate if the parent (the Tauri app) goes away. This is the
	// authoritative no-orphan guarantee for the desktop app: it fires no matter
	// HOW the parent died — clean quit, crash, force-quit, or SIGKILL — and does
	// not depend on the parent successfully signalling us (which is the fragile
	// path on macOS). The shell passes its PID via LEMA_PARENT_PID; if unset
	// (stdio/CLI use, or a hosted run) the watcher is a no-op.
	watchParentDeath()

	fmt.Fprintf(os.Stderr, "lema-mcp: workspace API on http://%s  (repo %q)\n", addr, repoName)
	fmt.Fprintf(os.Stderr, "lema-mcp: token %s  (Authorization: Bearer <token>, or ?token=)\n", token)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return srv.Serve(ln)
}

// httpInit runs lema-mcp's capture setup (init.go) in the engine's CWD — the repo
// it serves — so the GUI's "enable capture" registers lema in .mcp.json, writes the
// AGENTS.md capture protocol, and adds the commit-reminder hook (all idempotent,
// ADR-0042). Returns the labels of what it wrote (empty = already set up). POST
// only, because it mutates the user's repo.
func httpInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	wrote, err := initRepo(".")
	if err != nil {
		writeJSONResp(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSONResp(w, map[string]any{"ok": true, "wrote": wrote})
}

// watchParentDeath polls the parent PID (passed as LEMA_PARENT_PID by the Tauri
// shell) and exits the process when that parent is gone. It is the backstop for
// the orphaned-engine footgun: Tauri's child.kill() can be missed on macOS
// (tauri#9198, discussion #3273) and never runs on force-quit, but a dead parent
// is observable from here regardless. No-op when LEMA_PARENT_PID is unset or
// unparseable (non-desktop launches), so stdio/CLI/hosted behaviour is unchanged.
func watchParentDeath() {
	raw := strings.TrimSpace(os.Getenv("LEMA_PARENT_PID"))
	if raw == "" {
		return
	}
	ppid, err := strconv.Atoi(raw)
	if err != nil || ppid <= 1 {
		return
	}
	go func() {
		ticker := time.NewTicker(750 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			if !processAlive(ppid) {
				fmt.Fprintf(os.Stderr, "lema-mcp: parent %d gone, shutting down\n", ppid)
				os.Exit(0)
			}
		}
	}()
}

// processAlive reports whether pid is still a live process.
//
// Unix: signal 0 probes existence without delivering a signal — once the parent
// dies, Signal(0) returns ESRCH. This is the path that matters, because the
// macOS missed-kill bug is exactly where this backstop earns its keep.
//
// Windows: os.FindProcess succeeds even for dead PIDs and Signal(0) is not
// supported, so we cannot probe cheaply; we conservatively report "alive" and
// lean on the Rust-side kill (which is reliable on Windows — the orphan problem
// is macOS-specific). The watcher is thus a no-op on Windows, by design.
func processAlive(pid int) bool {
	if runtime.GOOS == "windows" {
		return true
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// httpToken returns LEMA_HTTP_TOKEN when set (so the GUI and server share a known
// value in dev), else a fresh random token printed on startup.
func httpToken() string {
	if t := strings.TrimSpace(os.Getenv("LEMA_HTTP_TOKEN")); t != "" {
		return t
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "lema-dev-token"
	}
	return hex.EncodeToString(b)
}

// withToken guards every /api/ route with the local token (Bearer header or
// ?token=). /healthz and CORS preflight stay open. A localhost write port that
// any local process or a drive-by page could POST to is a real surface (ADR-0044).
func withToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		// Constant-time compare so this local write port (ADR-0044) gives no
		// timing oracle on the token to a co-resident process or drive-by page.
		// ConstantTimeCompare returns 0 on a length mismatch, which is the correct
		// "unauthorized" outcome.
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withCORS allows the local GUI origin (localhost:3000 by default) to call the
// API from a browser. It validates the request Origin against LEMA_HTTP_ORIGIN
// (which can be a comma-separated list) to prevent overly permissive CORS.
func withCORS(next http.Handler) http.Handler {
	envOrigin := strings.TrimSpace(os.Getenv("LEMA_HTTP_ORIGIN"))
	if envOrigin == "" {
		envOrigin = "http://localhost:3000"
	}
	allowedOrigins := strings.Split(envOrigin, ",")
	for i := range allowedOrigins {
		allowedOrigins[i] = strings.TrimSpace(allowedOrigins[i])
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqOrigin := r.Header.Get("Origin")
		allowed := false

		// For requests without an Origin header (e.g., cURL, same-origin), we don't
		// enforce CORS origin restrictions but we still allow the request to proceed.
		if reqOrigin == "" {
			allowed = true
		} else {
			for _, o := range allowedOrigins {
				if o == "*" || o == reqOrigin {
					allowed = true
					// Echo the validated origin back
					w.Header().Set("Access-Control-Allow-Origin", reqOrigin)
					break
				}
			}
		}

		if reqOrigin != "" {
			w.Header().Set("Vary", "Origin")
		}

		// If it's a CORS preflight request and origin is not allowed, reject it.
		// If it's not allowed but it's a standard request, we just omit the CORS
		// headers and let the browser block the response.
		if allowed || reqOrigin == "" {
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}

		if r.Method == http.MethodOptions {
			if !allowed {
				http.Error(w, "CORS origin not allowed", http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSONResp encodes v into a buffer first and only writes to w if encoding
// succeeds, so a partially-written, corrupt JSON body can never reach the client
// (the same buffer-then-write pattern as writeJSON in init.go). Once a byte is
// written, header mutations are no-ops and appending an error string would
// corrupt the stream, so an encode failure is logged to stderr rather than turned
// into an http.Error on an already-committed response.
func writeJSONResp(w http.ResponseWriter, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("writeJSONResp: encode: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(buf.Bytes()); err != nil {
		log.Printf("writeJSONResp: write: %v", err)
	}
}

func queryInt(r *http.Request, key string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil {
		return n
	}
	return def
}

// mergedSearch runs the DecisionSource search and folds in this repo's captured
// decisions (ADR-0042): CLOSED matches lead, then the ADR atoms, then other
// captures. Shared by the stdio search_decisions tool and the HTTP API so both
// surfaces rank identically.
func mergedSearch(ctx context.Context, query string, k int) ([]source.Atom, error) {
	atoms, err := src.Search(ctx, query, k)
	if err != nil {
		return nil, err
	}
	if capture != nil {
		var closed, open []source.Atom
		for _, a := range capture.Search(query, k) {
			if a.Closed {
				closed = append(closed, a)
			} else {
				open = append(open, a)
			}
		}
		atoms = append(append(closed, atoms...), open...)
	}
	return atoms, nil
}

// httpSearch serves decision claims and/or project-doc chunks per the scope
// param (ADR-0055). The default is decisions ONLY — pinned by test — so the
// enforcement surfaces (EnforcementRail, check_decided callers) are provably
// unaffected by the docs feature.
func httpSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	k := queryInt(r, "k", 8)
	budget := queryInt(r, "max_tokens", 1500)
	scope := r.URL.Query().Get("scope") // "" | decisions | docs | all
	out := searchOutput{Repo: repoName, Claims: []source.Atom{}}
	if scope != "docs" {
		atoms, err := mergedSearch(r.Context(), q, k)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out.Claims, out.TokensUsed, out.Truncated = fitBudget(atoms, budget)
		out.Usage = localSearchROI(r.Context(), out.Claims)
	}
	if (scope == "docs" || scope == "all") && docsStore != nil {
		kept, used, trunc := fitDocsBudget(docsStore.Search(q, k), budget)
		out.Docs = kept
		out.TokensUsed += used
		out.Truncated = out.Truncated || trunc
	}
	writeJSONResp(w, out)
}

// httpDocs lists the chunk-indexed project docs for the workbench Docs tab.
// A nil store (hosted/remote run) serves an empty listing, not an error —
// the same stance as the empty-corpus serve mode.
func httpDocs(w http.ResponseWriter, _ *http.Request) {
	if docsStore == nil {
		writeJSONResp(w, map[string]any{"docs": []docs.Doc{}})
		return
	}
	writeJSONResp(w, map[string]any{"docs": docsStore.List()})
}

// httpDoc returns one doc — whole, or one section by heading — by its indexed
// relative path. The store lookup IS the traversal guard: content is served
// from memory keyed by the scanned path set, so a request-supplied path never
// touches the filesystem.
func httpDoc(w http.ResponseWriter, r *http.Request) {
	if docsStore == nil {
		http.Error(w, "no docs indexed", http.StatusNotFound)
		return
	}
	path := r.URL.Query().Get("path")
	d, body, ok := docsStore.Get(path)
	if !ok {
		http.Error(w, "doc not found", http.StatusNotFound)
		return
	}
	if sec := r.URL.Query().Get("section"); sec != "" {
		s, ok := docsStore.Section(path, sec)
		if !ok {
			http.Error(w, "section not found", http.StatusNotFound)
			return
		}
		body = s
	}
	// The HTTP default budget is the tool-response ceiling, not the MCP default:
	// the workbench renders whole docs; only the MCP tools default tight (1500).
	body, trunc := clipTokens(body, queryInt(r, "max_tokens", 25000))
	writeJSONResp(w, map[string]any{"doc": d, "body": body, "truncated": trunc})
}

func httpList(w http.ResponseWriter, r *http.Request) {
	out, err := src.List(r.Context(), r.URL.Query().Get("status"), queryInt(r, "limit", 50))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONResp(w, map[string]any{"decisions": out})
}

func httpGet(w http.ResponseWriter, r *http.Request) {
	n := queryInt(r, "number", 0)
	if n <= 0 {
		http.Error(w, "number is required and must be positive", http.StatusBadRequest)
		return
	}
	d, err := src.Get(r.Context(), n)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSONResp(w, map[string]any{"decision": d})
}

func httpGraph(w http.ResponseWriter, r *http.Request) {
	n := queryInt(r, "number", 0)
	if n <= 0 {
		http.Error(w, "number is required and must be positive", http.StatusBadRequest)
		return
	}
	g, err := src.Graph(r.Context(), n, queryInt(r, "depth", 1))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSONResp(w, map[string]any{"graph": g})
}

func httpCheck(w http.ResponseWriter, r *http.Request) {
	topic := r.URL.Query().Get("topic")
	var closed []source.Atom
	if capture != nil {
		closed = capture.CheckDecided(topic, 10)
	}
	writeJSONResp(w, checkOutput{Topic: topic, Decided: len(closed) > 0, Closed: closed})
}

// httpDecided returns every currently-CLOSED captured decision (rejected
// alternatives + superseded choices) with no query — the feed the cockpit's
// enforcement rail polls so a killed option surfaces the moment the agent records
// it, even from its separate stdio process.
func httpDecided(w http.ResponseWriter, _ *http.Request) {
	var closed []source.Atom
	if capture != nil {
		closed = capture.ClosedAtoms()
	}
	writeJSONResp(w, map[string]any{"closed": closed})
}

func httpRecord(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if capture == nil {
		http.Error(w, "capture store unavailable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var in source.DecisionRecord
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	rec, err := capture.Record(in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSONResp(w, map[string]any{"recorded": rec})
}
