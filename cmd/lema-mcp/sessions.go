package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// sessions.go backs the lema Workspaces "Sessions" feature (ADR-0046/0049): the
// GUI surface that lists the agent sessions on this machine — their title, the
// repo/branch they ran in, where the conversation "landed", and the decisions
// each session recorded — so the why-layer ties a captured decision back to the
// deliberation that produced it.
//
// Source of truth is the local agent transcript store, one JSONL file per
// session at ~/.claude/projects/<flattened-repo-path>/<session-uuid>.jsonl
// (one JSON object per line). The files are large (the worst case observed is a
// 16 MB / 4765-line transcript), so every scan is single-pass, streamed
// line-by-line, and substring-gated: a line is JSON-parsed ONLY when a cheap
// substring test says it could be one of the handful of records we care about
// (the first cwd/timestamp record, custom-title/ai-title, last-prompt, or a
// record_decision envelope). The giant assistant/tool_result bodies are skipped
// unparsed, so memory stays O(prompts-kept), not O(file).
//
// PRIVACY (a hard boundary, not a nicety). This API emits only DERIVED metadata.
// `landed` (the last user prompt) and `recentPrompts` are VERBATIM user prose,
// which can contain operational detail, pasted credentials, internal URLs, or
// customer data. So: (1) prompt text is hard-capped to a small length and run
// through a cheap secret-scrubber before it leaves SessionSource; (2)
// recentPrompts is OFF by default and only included on an explicit per-request
// opt-in (the GUI sets it on an open-this-session action, never on the list).
// The full transcript body is NEVER exposed, and ~/.claude.json (oauth / account
// / conversation history) is NEVER read. A hosted SessionSource inherits these
// same obligations — see the SessionSource doc comment — otherwise it would
// broadcast every engineer's raw prompts org-wide.
//
// OSS SEAM (ADR-0034). This file is in the PUBLIC cmd/lema-mcp binary: it imports
// only the Go standard library and same-package helpers (writeJSONResp, queryInt
// from serve.go). No apps/api/internal/* import. decisionID is reimplemented here
// (sessionDecisionID) to mirror the CaptureStore's content-keyed id so a
// Session's decision ref equals the .lema/decisions.jsonl record id and the GUI
// can deep-link — kept in sync by construction, not by import.

// SessionDecision is one decision a session recorded. ref is the content-keyed id
// (matches the CaptureStore's .lema/decisions.jsonl id when the title+chosen are
// recoverable, else ""). closed is always false from the transcript alone: a
// decision's CLOSED / superseded state lives in the CaptureStore, not in the
// session JSONL, so it is the documented join point — a caller that needs a
// truthful "closed" must cross-reference the CaptureStore by ref at read time.
type SessionDecision struct {
	Title  string `json:"title"`
	Ref    string `json:"ref"`
	Closed bool   `json:"closed"`
}

// SessionMeta is the per-session summary the list surface renders. All times are
// RFC3339 (or "" when no timestamp is recoverable and no mtime applies — in
// practice lastActive always has at least the file mtime).
type SessionMeta struct {
	ID            string            `json:"id"`            // session uuid == the .jsonl filename stem
	Title         string            `json:"title"`         // custom-title, else ai-title, else first user prompt, else "(untitled)"
	Repo          string            `json:"repo"`          // basename of the workspace root cwd, else the de-flattened project dir
	Branch        string            `json:"branch"`        // gitBranch ("" if none; "HEAD"/detached passes through)
	StartedAt     string            `json:"startedAt"`     // first in-file timestamp, RFC3339 ("" if none)
	LastActive    string            `json:"lastActive"`    // last in-file timestamp, else file mtime, RFC3339
	Landed        string            `json:"landed"`        // last-prompt text, capped + scrubbed ("" if none)
	DecisionCount int               `json:"decisionCount"` // true (uncapped) count of recorded decisions, after dedup
	Decisions     []SessionDecision `json:"decisions"`     // recorded-decision titles (capped in the list, full in detail)
}

// SessionDetail is one session in full for the open-this-session view. RecentPrompts
// is a few of the most-recent real user prompts (capped + scrubbed), included ONLY
// when the request opts in; it is never the full transcript. The embedded SessionMeta
// flattens into the same JSON object (so the wire shape is SessionMeta + recentPrompts).
type SessionDetail struct {
	SessionMeta
	RecentPrompts []string `json:"recentPrompts"`
	// Cwd is the session's original workspace-root directory (the shortest cwd seen
	// in the transcript). It is DETAIL-ONLY — never on the SessionMeta list payload —
	// so the cheap list call does not broadcast filesystem paths (and a future
	// HostedSessionSource inherits that). The GUI needs it to resume the session:
	// `claude --resume <id>` is directory-scoped, so it must be launched in this dir.
	Cwd string `json:"cwd"`
}

// SessionSource is the local/hosted seam (a first-class design requirement, not
// optional). The HTTP handlers depend ONLY on this interface, so the hosted build
// can swap the source without touching handlers or UI.
//
// LocalSessionSource (below) reads THIS machine's ~/.claude/projects.
//
// A future HostedSessionSource (NOT built here — the interface is left clean for
// it) would implement the SAME two methods against the organization's deployment,
// reading every engineer's sessions tied to the shared decision graph — "the gift
// of organizational knowledge, not just what's on your machine." id stays an
// opaque string so a server-side composite key (org+engineer+uuid) needs no
// signature change; ctx + limit already support server pagination and
// cancellation. CRITICALLY, a hosted impl inherits the same privacy boundary
// documented at the top of this file: it must cap + scrub prompt text and must
// not emit transcript bodies, or it broadcasts every engineer's raw prompts.
// The interface is deliberately FREE of any local transport detail. GetSession has
// no withPrompts flag: that opt-in is a property of the HTTP request (the ?prompts
// query param), not of the source. A source always returns a complete SessionDetail
// (recentPrompts already capped + scrubbed); the handler decides whether to forward
// recentPrompts to the client. This keeps the seam clean — a HostedSessionSource
// never has to know the query parameter exists.
type SessionSource interface {
	ListSessions(ctx context.Context, limit int) ([]SessionMeta, error)
	GetSession(ctx context.Context, id string) (SessionDetail, error)
}

// sessionSrc is the package-level source the handlers use. It is a var so a hosted
// build can replace it (mirroring main.go's existing src/capture wiring); the
// default reads this machine. If the home directory is unresolvable the root is
// "" and the source serves an empty list — never fatal, consistent with serve
// mode's "empty corpus must not be fatal" rule.
var sessionSrc SessionSource = newDefaultLocalSessionSource()

// newDefaultLocalSessionSource builds the local source rooted at the default
// ~/.claude/projects path. An unresolvable home yields an empty-rooted source
// that lists nothing rather than erroring at construction.
func newDefaultLocalSessionSource() SessionSource {
	root, _ := defaultSessionsRoot() // empty root on error -> empty list, never fatal
	return NewLocalSessionSource(root)
}

// errNoHome is returned by defaultSessionsRoot when the user's home directory
// cannot be resolved. Generic (no paths/secrets) so it is safe to surface.
var errNoHome = errors.New("home directory unavailable")

// errNotFound is the sentinel GetSession returns when no transcript matches the
// id; the handler maps it to 404 (matching httpGet's convention). Generic string,
// no path leaked.
var errNotFound = errors.New("session not found")

// defaultSessionsRoot returns the default sessions root (~/.claude/projects). This
// is the ONLY place the home-directory assumption lives — handlers and any future
// hosted impl see only an injected `root`, so the local-filesystem assumption
// never leaks past the SessionSource boundary.
func defaultSessionsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errNoHome
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// LocalSessionSource reads sessions from a single directory of project folders
// (root/<flattened-repo>/<uuid>.jsonl). root is INJECTED, never hardcoded, so the
// home-path assumption stays quarantined in defaultSessionsRoot.
type LocalSessionSource struct {
	root string
}

// NewLocalSessionSource constructs a LocalSessionSource. An empty root is valid and
// yields an empty session list (the not-configured / no-home case).
func NewLocalSessionSource(root string) *LocalSessionSource {
	return &LocalSessionSource{root: root}
}

var _ SessionSource = (*LocalSessionSource)(nil)

// sessionIDPattern is the strict allowlist for a session id. It is positive
// (allowlist) rather than a "/", "\\", ".." blacklist: a value that matches this
// structurally CANNOT contain a path separator or "..", so it cannot traverse out
// of root. The real session uuids satisfy it; it is also wide enough to admit a
// future hosted composite key (org_engineer_uuid) without banning "..". Anything
// else is rejected 400 before a path is ever built.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// validSessionID reports whether id is a clean session id safe to map to a
// filename. The handler relies on r.URL.Query().Get already percent-decoding the
// value, and this allowlist is the positive guard on the decoded result.
func validSessionID(id string) bool {
	return sessionIDPattern.MatchString(id)
}

// scannerBufCap sizes the bufio.Reader buffer and is the threshold above which a
// single JSONL line is treated as oversized. A small default (e.g. 64 KiB) would be
// far too small — single transcript lines routinely exceed it (the max observed is
// ~951 KiB). The largest single line observed is under 8 MiB, but lastPrompt and
// pasted content are unbounded, so a line longer than this is handled explicitly
// (drained-and-skipped by readScanLine) rather than truncating the scan.
const scannerBufCap = 8 * 1024 * 1024

// promptCap is the hard maximum length of any user-prompt-derived string that
// leaves this boundary (landed, a prompt-derived title, a recentPrompts entry).
// Prompt text is sensitive content, not a label: capping bounds how much verbatim
// prose can leak. 200 is enough to be a useful summary line, small enough not to
// dump a whole prompt.
const promptCap = 200

// listDecisionCap bounds how many decisions[] entries the LIST response carries
// per session (the detail response returns the full deduped slice). decisionCount
// stays exact regardless.
const listDecisionCap = 5

// recentPromptCount is how many most-recent real user prompts the detail response
// carries when prompts are opted in. Kept small — these are sensitive verbatim
// prose, and the ring buffer that collects them is sized to this so detail memory
// stays O(recentPromptCount), not O(file).
const recentPromptCount = 5

// maxListScan caps how many candidate files the list path fully scans, after
// sorting by mtime — a defensive bound so a machine with thousands of sessions
// cannot turn one /api/sessions call into thousands of file scans. The handler's
// limit is applied on top; this is the hard ceiling.
const maxListScan = 200

// ListSessions returns the most-recently-active sessions first, capped to limit.
// It is cheap by design: stat every root/<dir>/<uuid>.jsonl (single level — it
// must NOT recurse; the nested subagents/*.jsonl transcripts are intentionally
// excluded by depth), sort by mtime descending, then run the single-pass cheap
// scanner only on the top `limit` (bounded by maxListScan). A missing root is an
// empty list, never an error — the not-configured / fresh-machine case.
func (s *LocalSessionSource) ListSessions(ctx context.Context, limit int) ([]SessionMeta, error) {
	if s.root == "" {
		return []SessionMeta{}, nil
	}
	if limit <= 0 || limit > maxListScan {
		limit = maxListScan
	}

	// Single-level glob: root/<project-dir>/<uuid>.jsonl. Globbing two segments
	// (not filepath.WalkDir) is what keeps this to top-level transcripts and
	// excludes the nested subagents/ trees by construction.
	paths, err := filepath.Glob(filepath.Join(s.root, "*", "*.jsonl"))
	if err != nil {
		// A malformed glob pattern is not the user's problem and leaks nothing
		// useful; degrade to empty rather than 500.
		return []SessionMeta{}, nil
	}
	if len(paths) == 0 {
		// Root absent or empty -> empty list (200), per the error contract.
		return []SessionMeta{}, nil
	}

	// Stat first so we can sort by mtime and only scan the freshest `limit`.
	type fileStat struct {
		path string
		mod  time.Time
	}
	stats := make([]fileStat, 0, len(paths))
	for _, p := range paths {
		info, statErr := os.Stat(p)
		if statErr != nil {
			continue // vanished between glob and stat; skip
		}
		stats = append(stats, fileStat{path: p, mod: info.ModTime()})
	}
	sort.Slice(stats, func(i, j int) bool { return stats[i].mod.After(stats[j].mod) })

	out := make([]SessionMeta, 0, limit)
	for _, fs := range stats {
		if len(out) >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		meta, _, _, skip := scanSession(fs.path, fs.mod, false)
		if skip {
			continue // every record was a sidechain (sub-agent transcript), not a top-level session
		}
		// Cap decisions[] in the list to stay light; decisionCount is already exact.
		if len(meta.Decisions) > listDecisionCap {
			meta.Decisions = meta.Decisions[:listDecisionCap]
		}
		out = append(out, meta)
	}
	return out, nil
}

// GetSession returns one session in full, including recentPrompts (already capped +
// secret-scrubbed). The recentPrompts opt-in is enforced by the HTTP handler, NOT
// here: the source always returns the complete detail so the seam carries no
// transport detail (see the SessionSource doc comment). The id is allowlist-
// validated and the resolved path is asserted to stay within root (defense in
// depth) before any read. A nonexistent session is errNotFound (404).
func (s *LocalSessionSource) GetSession(ctx context.Context, id string) (SessionDetail, error) {
	if !validSessionID(id) {
		return SessionDetail{}, errNotFound
	}
	if s.root == "" {
		return SessionDetail{}, errNotFound
	}

	// The id maps to a filename, so it is a path-traversal vector. We never build
	// the path from URL.Path or a re-encoded source — only from the decoded,
	// allowlist-validated query value — and we still re-check containment below.
	matches, err := filepath.Glob(filepath.Join(s.root, "*", id+".jsonl"))
	if err != nil || len(matches) == 0 {
		return SessionDetail{}, errNotFound
	}

	// Resolve the absolute root once so the prefix check is meaningful even if root
	// was given as a relative path.
	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return SessionDetail{}, errNotFound
	}
	rootPrefix := absRoot + string(os.PathSeparator)

	var path string
	for _, m := range matches {
		absM, absErr := filepath.Abs(m)
		if absErr != nil {
			continue
		}
		// Defense in depth: the cleaned, absolute candidate must live under root.
		// The allowlist already forbids separators in id, so this can only fail if
		// the glob itself returned something unexpected — fail closed.
		if !strings.HasPrefix(absM, rootPrefix) {
			continue
		}
		path = m
		break
	}
	if path == "" {
		return SessionDetail{}, errNotFound
	}

	if err := ctx.Err(); err != nil {
		return SessionDetail{}, err
	}

	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionDetail{}, errNotFound
		}
		// A real read error of an existing file: generic, no path leaked.
		return SessionDetail{}, errors.New("could not read session")
	}

	// Always collect prompts here; the handler zeroes them when ?prompts is unset.
	meta, prompts, cwd, skip := scanSession(path, info.ModTime(), true)
	if skip {
		return SessionDetail{}, errNotFound
	}
	detail := SessionDetail{SessionMeta: meta, RecentPrompts: prompts, Cwd: cwd}
	if detail.RecentPrompts == nil {
		detail.RecentPrompts = []string{}
	}
	return detail, nil
}

// jsonlRecord is the SUPERSET of transcript fields this feature reads. Every field
// is explicitly typed so unknown keys are dropped (the same defensive,
// drop-unknown-keys style plugins.go uses): nothing of the raw record is echoed
// back, only the derived fields below. message is decoded lazily (json.RawMessage)
// because most records that reach json.Unmarshal here are decision candidates
// whose message body we must inspect, but for title/landed records the body is
// irrelevant and we skip decoding it.
type jsonlRecord struct {
	Type                 string          `json:"type"`
	SessionID            string          `json:"sessionId"`
	Cwd                  string          `json:"cwd"`
	GitBranch            string          `json:"gitBranch"`
	Timestamp            string          `json:"timestamp"`
	IsSidechain          bool            `json:"isSidechain"`
	IsMeta               bool            `json:"isMeta"`
	CustomTitle          string          `json:"customTitle"`
	AiTitle              string          `json:"aiTitle"`
	LastPrompt           string          `json:"lastPrompt"`
	AttributionMcpServer string          `json:"attributionMcpServer"`
	AttributionMcpTool   string          `json:"attributionMcpTool"`
	ToolUseResult        json.RawMessage `json:"toolUseResult"`
	Message              json.RawMessage `json:"message"`
}

// assistantMessage is the minimal shape of an assistant record's message we need
// to find tool_use blocks. content is a heterogeneous list (text / tool_use /
// tool_result blocks); we decode each block's type/name/input only.
type assistantMessage struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Text  string          `json:"text"`
	Input json.RawMessage `json:"input"`
}

// userMessage is the minimal shape of a user record's message. content is either a
// plain string or a list of blocks (text / tool_result); we extract the text.
type userMessage struct {
	Content json.RawMessage `json:"content"`
}

// scanSession does the single, substring-gated pass over one transcript file and
// returns its SessionMeta. It is shared by the list and detail paths; collectPrompts
// turns on the recentPrompts ring buffer (detail only). The cwd return is the
// workspace-root directory (the shortest cwd seen across the transcript; "" when
// none was recoverable) — the detail path surfaces it so the GUI can resume the
// session in the right directory, the list path ignores it. The final (skip) return
// is true when the file should be SKIPPED (every record was a sidechain sub-agent
// transcript — belt-and-suspenders; top-level files are never sidechain-first in
// real data).
//
// EFFICIENCY: for each line a cheap substring test runs FIRST; json.Unmarshal is
// called ONLY on lines that could be a record we care about. The bulk
// assistant/tool_result bodies match nothing and are skipped unparsed.
//
// We read with a bufio.Reader (NOT bufio.Scanner) precisely so an oversized line
// can be DRAINED and scanning genuinely CONTINUES. bufio.Scanner is unrecoverable
// after bufio.ErrTooLong (it cannot advance past the offending token — Go issue
// #26431), so a single >scannerBufCap line would have silently truncated the rest
// of the scan and dropped every later title/landed/decision. With the reader we
// detect a line that grows past scannerBufCap, discard the remaining bytes up to
// the next newline, skip just that one line, and keep going — honouring the
// project's "fail loud" rule (a silently short-scanned session is exactly the
// skip-this-silently failure the rule forbids).
func scanSession(path string, mtime time.Time, collectPrompts bool) (SessionMeta, []string, string, bool) {
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")

	meta := SessionMeta{
		ID:         id,
		Decisions:  []SessionDecision{},
		LastActive: mtime.UTC().Format(time.RFC3339),
	}

	f, err := os.Open(path)
	if err != nil {
		// Unreadable file: return the cheap id+mtime meta rather than dropping the
		// session entirely. Title falls back to "(untitled)" below.
		meta.Title = "(untitled)"
		meta.Repo = deFlattenProjectDir(path)
		return meta, nil, "", false
	}
	defer f.Close()

	// bufio.Reader (not Scanner): a line up to scannerBufCap is returned by a
	// single ReadLine call (isPrefix=false); a longer line returns isPrefix=true and
	// is DRAINED (see readScanLine) so the scan recovers and continues past it
	// instead of truncating — unlike bufio.Scanner, which is dead after ErrTooLong.
	br := bufio.NewReaderSize(f, scannerBufCap)

	var (
		customTitle  string // last custom-title wins (records repeat, carry no timestamp)
		aiTitle      string // last ai-title wins
		firstPrompt  string // first real user prompt (title fallback)
		lastTS       string // last timestamp seen on any parsed record
		gotCwd       bool   // repo/branch/startedAt locked once first cwd-bearing record is seen
		shortestCwd  string // workspace-root heuristic: the shortest cwd seen
		anyRecord    bool   // saw at least one parseable record
		allSidechain = true // becomes false the moment a non-sidechain record is seen

		decisions  []SessionDecision
		decSeen    = map[string]bool{} // dedup key set (ref, else lower(title))
		promptRing []string            // detail-only ring buffer of recent prompts
	)

	for {
		line, oversized, eof := readScanLine(br)
		if oversized {
			// One line exceeded scannerBufCap; it was fully drained to the next
			// newline. Skip just this line and keep scanning so a single giant
			// tool_result cannot truncate the title/landed/decision records after it.
			if eof {
				break // the oversized line ran to EOF with no trailing newline
			}
			continue
		}
		if eof && len(line) == 0 {
			break // clean end of file
		}
		if len(line) == 0 {
			if eof {
				break
			}
			continue
		}

		// --- CHEAP SUBSTRING GATE (no JSON parse unless a marker is present) ---
		// cwd is sampled on EVERY cwd-bearing line (not just title/decision lines):
		// cwd VARIES within a session as the agent cd's into subdirs, so the shortest
		// cwd across the WHOLE transcript is the workspace root. It is pulled with a
		// direct substring read (indexCwdValue) — cheaper than a struct unmarshal and,
		// crucially, it does NOT require the rest of the record to be parsed, so a
		// giant assistant/tool_result body that merely carries a top-level cwd still
		// contributes its cwd without a full-record parse.
		hasCwd := bytesContains(line, `"cwd":"`)
		if hasCwd {
			if c := indexCwdValue(line); c != "" {
				if shortestCwd == "" || len(c) < len(shortestCwd) {
					shortestCwd = c
				}
			}
		}
		isCustom := bytesContains(line, `"type":"custom-title"`)
		isAiTitle := bytesContains(line, `"type":"ai-title"`)
		isLast := bytesContains(line, `"type":"last-prompt"`)
		// Decision gate: a bare "record_decision" substring is prose noise (the repo
		// authors its own tooling), so it is NECESSARY but not SUFFICIENT — the
		// structural validation after parse is the real filter. We narrow the parse
		// set tightly: Form A needs the literal "type":"tool_use" envelope on the same
		// line as record_decision (so prose and tool_result OUTPUT bodies — which
		// mention record_decision and "tool_use" but are not themselves a tool_use
		// block — do not pass); Form B needs the literal attribution-tool field.
		maybeFormA := bytesContains(line, `"type":"tool_use"`) &&
			bytesContains(line, "record_decision")
		maybeFormB := bytesContains(line, `"attributionMcpTool":"record_decision"`)
		// We still need the FIRST cwd-bearing record parsed once, to lock branch +
		// startedAt off the same record (those are not substring-extractable). After
		// that, gotCwd is set and bulk cwd-only lines fall through to the skip below.
		needCwdRecord := !gotCwd && hasCwd
		// A user line is parsed only when we still need the first prompt, or detail
		// is collecting the recent-prompt ring.
		isUser := bytesContains(line, `"type":"user"`)
		needUser := isUser && (firstPrompt == "" || collectPrompts)

		if !needCwdRecord && !isCustom && !isAiTitle && !isLast && !maybeFormA && !maybeFormB && !needUser {
			continue // bulk message body — skip unparsed (the efficiency win)
		}

		var rec jsonlRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			continue // tolerate a malformed line; degrade, never panic
		}
		anyRecord = true
		if !rec.IsSidechain {
			allSidechain = false
		}
		if rec.Timestamp != "" {
			lastTS = rec.Timestamp
		}

		// repo / branch / startedAt: lock branch + startedAt from the FIRST cwd-bearing
		// record (line 1 is often a bare {"type":"mode"} with no cwd/timestamp, so we
		// scan forward). shortestCwd is already maintained by the substring path above
		// across EVERY cwd-bearing line, so it is not touched here.
		if needCwdRecord && rec.Cwd != "" {
			gotCwd = true
			meta.Branch = rec.GitBranch
			if rec.Timestamp != "" && meta.StartedAt == "" {
				meta.StartedAt = rec.Timestamp
			}
		}
		// startedAt: if the first cwd record had no timestamp, take the first
		// timestamp from any later record.
		if meta.StartedAt == "" && rec.Timestamp != "" {
			meta.StartedAt = rec.Timestamp
		}

		switch {
		case isCustom && rec.CustomTitle != "":
			customTitle = rec.CustomTitle
		case isAiTitle && rec.AiTitle != "":
			aiTitle = rec.AiTitle
		case isLast && rec.LastPrompt != "":
			meta.Landed = sanitizePrompt(rec.LastPrompt)
		}

		// Decision extraction (structural validation — the load-bearing part).
		if maybeFormA || maybeFormB {
			if d, ok := extractDecision(rec); ok {
				key := d.Ref
				if key == "" {
					key = strings.ToLower(strings.TrimSpace(d.Title))
				}
				if key != "" && !decSeen[key] {
					decSeen[key] = true
					decisions = append(decisions, d)
				}
			}
		}

		// User-prompt handling (first-prompt title fallback + detail ring buffer).
		if needUser && rec.Type == "user" && !rec.IsMeta {
			if text := userPromptText(rec.Message); text != "" {
				if firstPrompt == "" {
					firstPrompt = text
				}
				if collectPrompts {
					promptRing = append(promptRing, text)
					if len(promptRing) > recentPromptCount {
						promptRing = promptRing[1:]
					}
				}
			}
		}
	}

	// Skip a file whose every record was a sidechain sub-agent transcript.
	if anyRecord && allSidechain {
		return SessionMeta{}, nil, "", true
	}

	// repo: basename of the workspace-root cwd, else de-flatten the project dir.
	if shortestCwd != "" {
		meta.Repo = filepath.Base(shortestCwd)
	} else {
		meta.Repo = deFlattenProjectDir(path)
	}

	// lastActive: prefer the actual last in-file timestamp (more precise than
	// mtime); fall back to the mtime already set.
	if lastTS != "" {
		if t, perr := time.Parse(time.RFC3339, lastTS); perr == nil {
			meta.LastActive = t.UTC().Format(time.RFC3339)
		}
	}

	// title: custom-title -> ai-title -> first real user prompt -> "(untitled)".
	switch {
	case customTitle != "":
		meta.Title = trimTitle(customTitle)
	case aiTitle != "":
		meta.Title = trimTitle(aiTitle)
	case firstPrompt != "":
		meta.Title = firstPrompt // already sanitized + capped by userPromptText
	default:
		meta.Title = "(untitled)"
	}

	meta.Decisions = decisions
	meta.DecisionCount = len(decisions)
	if meta.Decisions == nil {
		meta.Decisions = []SessionDecision{}
	}

	var prompts []string
	if collectPrompts {
		prompts = promptRing
	}
	return meta, prompts, shortestCwd, false
}

// extractDecision validates a prefiltered record as a real record_decision and
// pulls out its title/ref. The substring gate is necessary but NOT sufficient —
// "record_decision" appears in Edit/Write/Bash inputs, assistant text, and
// tool_result bodies because the repo authors its own tooling — so a candidate
// counts ONLY if it structurally matches one of the two real shapes. (Validated:
// in the local corpus, 98 lines pass the Form-A substring gate yet ZERO have an
// actual tool_use block named record_decision; structural validation rejects all
// of them. There are currently zero genuine record_decision invocations anywhere
// in the corpus — capture is new — so this legitimately returns nothing today and
// is built to be correct WHEN real calls appear.)
func extractDecision(rec jsonlRecord) (SessionDecision, bool) {
	// FORM A — assistant tool_use (primary).
	if rec.Type == "assistant" && len(rec.Message) > 0 {
		var msg assistantMessage
		if err := json.Unmarshal(rec.Message, &msg); err == nil {
			for _, b := range msg.Content {
				if b.Type != "tool_use" {
					continue
				}
				if !isRecordDecisionTool(b.Name) {
					continue
				}
				title, chosen := titleFromInput(b.Input)
				if title == "" {
					title = "(decision)"
				}
				return newSessionDecision(title, chosen), true
			}
		}
	}

	// FORM B — top-level attribution record (fallback / older transcripts).
	if (rec.AttributionMcpServer == "lema" || rec.AttributionMcpServer == "cairn") &&
		rec.AttributionMcpTool == "record_decision" {
		title, chosen := titleFromResult(rec.ToolUseResult)
		if title == "" {
			title = "(decision)"
		}
		return newSessionDecision(title, chosen), true
	}

	return SessionDecision{}, false
}

// isRecordDecisionTool matches a tool name whose suffix is record_decision after
// stripping an optional "mcp__<server>__" prefix, so any server alias works
// (record_decision, mcp__lema__record_decision, mcp__cairn__record_decision, ...).
func isRecordDecisionTool(name string) bool {
	name = strings.TrimSpace(name)
	if name == "record_decision" {
		return true
	}
	if i := strings.LastIndex(name, "__"); i >= 0 {
		return name[i+2:] == "record_decision"
	}
	return false
}

// newSessionDecision builds a SessionDecision, computing ref as the SAME content-
// keyed id the CaptureStore uses (so the GUI can deep-link a Session's decision to
// its .lema/decisions.jsonl record) when both title and chosen are recoverable.
// closed is always false: a freshly-recorded decision is "accepted"; its CLOSED /
// superseded state is owned by the CaptureStore and is not knowable from the
// transcript line (the documented cross-reference-by-ref join point).
func newSessionDecision(title, chosen string) SessionDecision {
	ref := ""
	if title != "" && chosen != "" {
		ref = sessionDecisionID(title, chosen)
	}
	return SessionDecision{Title: trimTitle(title), Ref: ref, Closed: false}
}

// sessionDecisionID mirrors source.decisionID byte-for-byte (it cannot be imported
// across the OSS seam): fnv32a over lower(trim(title)) + 0x00 + lower(trim(chosen)),
// formatted d_%06x masked to the low 24 bits. Kept in sync by construction so a
// Session's decision ref EQUALS the CaptureStore record id.
func sessionDecisionID(title, chosen string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(title))))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strings.ToLower(strings.TrimSpace(chosen))))
	return fmt.Sprintf("d_%06x", h.Sum32()&0xffffff)
}

// titleFromInput pulls (title, chosen) from a tool_use input object. Title source,
// in order: title -> topic -> decision -> chosen -> a short snippet of the trimmed
// input JSON. chosen is the input's "chosen" field ("chose" is accepted as an
// alias). Both are best-effort; an unparseable input yields ("", "").
func titleFromInput(raw json.RawMessage) (title, chosen string) {
	if len(raw) == 0 {
		return "", ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", ""
	}
	chosen = firstString(m, "chosen", "chose")
	title = firstString(m, "title", "topic", "decision", "chosen")
	if title == "" {
		title = snippet(raw)
	}
	return title, chosen
}

// titleFromResult pulls (title, chosen) from a Form-B toolUseResult, which is
// observed as EITHER a JSON object OR a JSON string — type-switch on it. Title
// source for the object form: title -> topic -> decision -> chosen -> chose; for
// the string form, a trimmed snippet of the string.
func titleFromResult(raw json.RawMessage) (title, chosen string) {
	if len(raw) == 0 {
		return "", ""
	}
	// Object form.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil && m != nil {
		chosen = firstString(m, "chosen", "chose")
		title = firstString(m, "title", "topic", "decision", "chosen", "chose")
		return title, chosen
	}
	// String form.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return snippetStr(s), ""
	}
	return "", ""
}

// firstString returns the first non-empty string value among keys, trimmed.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				if t := strings.TrimSpace(s); t != "" {
					return t
				}
			}
		}
	}
	return ""
}

// snippet returns a short, single-line label from raw JSON bytes (last-resort
// title when no named field is present). Capped at promptCap and scrubbed, since
// the raw input could contain prompt-like prose.
func snippet(raw json.RawMessage) string {
	return snippetStr(string(raw))
}

func snippetStr(s string) string {
	return sanitizePrompt(s)
}

// userPromptText extracts the user-authored text from a user record's message,
// then sanitizes it (cap + scrub). It returns "" for turns that are NOT real user
// prose — tool_result-only turns (no text block) and system-injected envelopes
// whose text begins with an angle-bracket tag (<command-name>, <command-message>,
// <local-command...>, <task-notification>, etc.) — so the title/recentPrompts
// never surface those.
func userPromptText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var um userMessage
	if err := json.Unmarshal(raw, &um); err != nil {
		return ""
	}
	text := extractContentText(um.Content)
	text = strings.TrimSpace(text)
	if text == "" {
		return "" // tool_result-only turn (no authored text)
	}
	// System-injected envelopes all open with an angle-bracket tag; skip them.
	if strings.HasPrefix(text, "<") {
		return ""
	}
	return sanitizePrompt(text)
}

// extractContentText returns the concatenated text of a message content value,
// which is either a plain string or a list of blocks (only "text" blocks
// contribute; tool_result/image blocks are ignored).
func extractContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// String form.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	// List-of-blocks form.
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}

// trimTitle normalizes a title (collapse whitespace to single spaces) and caps it
// to promptCap. Titles from custom-title/ai-title are user/model-authored labels,
// not raw prompts, so they are capped but NOT secret-scrubbed (scrubbing a
// deliberate title would corrupt it); prompt-derived titles are scrubbed upstream
// by userPromptText.
func trimTitle(s string) string {
	s = strings.TrimSpace(collapseWS(s))
	return capLen(s, promptCap)
}

// sanitizePrompt is the privacy gate for any user-prompt-derived string that
// leaves this boundary: collapse whitespace to a single line, redact obvious
// secrets, then hard-cap the length. Applied to landed, prompt-derived titles,
// recentPrompts, and last-resort snippets.
func sanitizePrompt(s string) string {
	s = collapseWS(s)
	s = scrubSecrets(s)
	s = strings.TrimSpace(s)
	return capLen(s, promptCap)
}

// collapseWS replaces any run of whitespace (newlines, tabs, spaces) with a single
// space, so a multi-line prompt becomes one tidy line.
func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// capLen truncates s to at most n bytes on a rune boundary, appending a single-
// character ellipsis when it cuts. Operates on runes so it never splits a UTF-8
// sequence.
func capLen(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// secretPattern matches common credential shapes so prompt text cannot leak a
// pasted key/token/password through landed/recentPrompts. It is a cheap, best-
// effort scrub (NOT a guarantee — the privacy boundary is also "prompts off by
// default + hard cap"), tuned to high-signal prefixes: provider key prefixes
// (sk-/ghp_/ghs_/github_pat_/gho_/glpat-/xox[abprs]-/AKIA + AWS secret-ish runs),
// bearer headers, and key=value assignments naming a secret.
var secretPattern = regexp.MustCompile(
	`(?i)` +
		`(sk-[A-Za-z0-9_\-]{8,})` +
		`|(ghp_[A-Za-z0-9]{16,})` +
		`|(gho_[A-Za-z0-9]{16,})` +
		`|(ghs_[A-Za-z0-9]{16,})` +
		`|(github_pat_[A-Za-z0-9_]{20,})` +
		`|(glpat-[A-Za-z0-9_\-]{16,})` +
		`|(xox[abprs]-[A-Za-z0-9\-]{8,})` +
		`|(AKIA[0-9A-Z]{16})` +
		`|(bearer\s+[A-Za-z0-9._\-]{12,})` +
		`|(eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{6,})` + // JWT
		`|((?:password|passwd|secret|token|api[_\-]?key|access[_\-]?key)\s*[:=]\s*\S{4,})`,
)

// scrubSecrets replaces any matched credential-shaped substring with [redacted],
// so a prompt that pasted a key does not ship its opening bytes out of the API.
func scrubSecrets(s string) string {
	return secretPattern.ReplaceAllString(s, "[redacted]")
}

// deFlattenProjectDir derives a repo name from the project DIRECTORY name when no
// cwd is recoverable from the transcript (last resort). Claude flattens a repo
// path into a dir like "-Users-andrew-lema-lema"; we strip the leading "-" and
// take the last path segment ("lema"). Returns "" if nothing usable.
func deFlattenProjectDir(path string) string {
	dir := filepath.Base(filepath.Dir(path))
	dir = strings.TrimPrefix(dir, "-")
	if dir == "" {
		return ""
	}
	parts := strings.Split(dir, "-")
	return parts[len(parts)-1]
}

// readScanLine reads one logical line from br, WITHOUT the trailing newline. It is
// the recovery-capable replacement for bufio.Scanner.Scan: a line up to
// scannerBufCap is returned whole (oversized=false); a line longer than the
// reader's buffer is DRAINED to the next newline and reported as oversized=true with
// a nil line, so the caller skips exactly that one line and keeps scanning. eof is
// true once the underlying reader is exhausted. The returned slice aliases br's
// internal buffer and is only valid until the next read (same contract as
// Scanner.Bytes), which is why an oversized line — whose chunks are invalidated as
// we drain — is discarded rather than assembled.
func readScanLine(br *bufio.Reader) (line []byte, oversized bool, eof bool) {
	first, isPrefix, err := br.ReadLine()
	if err != nil {
		// io.EOF (or any read error) with no bytes: clean end. ReadLine never returns
		// both data and io.EOF in the same call, so first is empty here.
		return nil, false, true
	}
	if !isPrefix {
		// Whole line fit in the buffer.
		return first, false, false
	}
	// The line is longer than the buffer (would have been bufio.ErrTooLong under a
	// Scanner). Drain the remaining chunks to the next newline and skip the line —
	// it is necessarily one of the giant assistant/tool_result bodies we never parse.
	for {
		_, isPrefix, err = br.ReadLine()
		if err != nil {
			return nil, true, true // oversized line ran to EOF with no trailing newline
		}
		if !isPrefix {
			return nil, true, false // reached the end of the oversized line
		}
	}
}

// indexCwdValue extracts the cwd string value from a transcript line by direct
// substring read — no json.Unmarshal, so it stays cheap even when cwd sits at the
// far end of a 150 KB record (observed) and never forces a full-record parse. It
// finds the first `"cwd":"` and returns the bytes up to the next unescaped quote.
// A backslash escapes the following byte (so a path containing \" is not truncated
// early); cwd values are filesystem paths and in practice contain no escapes, and
// any imperfect extraction only affects the best-effort repo LABEL, never
// correctness or the privacy boundary. Returns "" when no cwd value is present.
func indexCwdValue(line []byte) string {
	const marker = `"cwd":"`
	s := bytesToStringUnsafe(line)
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	for j := 0; j < len(rest); j++ {
		switch rest[j] {
		case '\\':
			j++ // skip the escaped byte
		case '"':
			return rest[:j]
		}
	}
	return "" // unterminated; treat as absent rather than returning a partial path
}

// bytesContains is a tiny helper so the hot substring gate reads cleanly without
// allocating a string per line.
func bytesContains(b []byte, sub string) bool {
	return strings.Contains(bytesToStringUnsafe(b), sub)
}

// bytesToStringUnsafe converts the line bytes to a string for read-only substring
// work within the same loop iteration. The result is never retained past the
// iteration (the backing array aliases the bufio.Reader buffer and is valid only
// until the next read), so this is safe. Kept simple and explicit rather than
// importing unsafe; strings.Contains over a fresh conversion is the conservative,
// allocation-but-correct choice here.
func bytesToStringUnsafe(b []byte) string {
	return string(b)
}

// ---- HTTP handlers (exported; the parent registers the routes in serve.go) ----

// httpSessions (GET /api/sessions) returns the machine's sessions, most-recent
// first, capped by ?limit (default/maximum 200). The list NEVER includes
// recentPrompts (sensitive verbatim prose — that is detail-only, opt-in). Missing
// ~/.claude/projects degrades to an empty list (200), never 500. withToken +
// withCORS (serve.go) already wrap this route.
func httpSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	out, err := sessionSrc.ListSessions(r.Context(), queryInt(r, "limit", maxListScan))
	if err != nil {
		// Generic message — no path/secret. Any genuine failure is a server error;
		// the absent-corpus case already returned an empty list, not an error.
		http.Error(w, "could not list sessions", http.StatusInternalServerError)
		return
	}
	if out == nil {
		out = []SessionMeta{}
	}
	writeJSONResp(w, map[string]any{"sessions": out})
}

// httpSession (GET /api/session?id=<uuid>) returns one session in full. The id is
// allowlist-validated (path-traversal guard) before any file path is built; a
// missing id is 400, an unknown/invalid id is 404. recentPrompts (sensitive
// verbatim user prose) is included ONLY when ?prompts=1|true is set — the GUI sets
// it on an explicit open-this-session action, never on the list — and is always
// capped + secret-scrubbed.
func httpSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	// Reject a malformed id up front with a 400 (the source also guards, returning
	// not-found; a clearly-bad shape is more honestly a bad request).
	if !validSessionID(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	detail, err := sessionSrc.GetSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, errNotFound) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		http.Error(w, "could not read session", http.StatusInternalServerError)
		return
	}
	// Privacy gate lives HERE, above the seam: recentPrompts (verbatim user prose) is
	// only forwarded on an explicit ?prompts=1|true opt-in. Without it, drop the
	// prompts the source collected so the list/default detail never ships them. The
	// field stays present as [] so the wire shape is stable.
	if !queryBool(r, "prompts") {
		detail.RecentPrompts = []string{}
	}
	writeJSONResp(w, map[string]any{"session": detail})
}

// queryBool reports whether a query flag is set truthy ("1", "true", "yes", "on").
// Absent or any other value is false — recentPrompts stays off unless explicitly
// requested.
func queryBool(r *http.Request, key string) bool {
	switch strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
