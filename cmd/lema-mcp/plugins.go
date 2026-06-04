package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// plugins.go backs the lema Workspaces "Plugins" control panel (ADR-0043/0044):
// the GUI surface that shows, for the served repo, which agents are present,
// which MCP servers and capture hooks are wired, and which projection files
// (CLAUDE.md / AGENTS.md / MEMORY.md) exist. GET reports a truthful snapshot;
// POST toggles the two things that have a real, non-destructive write here — MCP
// servers (via disabledMcpjsonServers) and the lema commit-reminder hook — and
// returns the fresh snapshot so the UI re-renders against reality. Agent rows
// and projections are read-only status; the honest-UI rule (ADR-0044) forbids a
// switch where no write exists.
//
// Security (ADR-0044/0048): reads the served repo's .mcp.json,
// .claude/settings.json, and projection markdown files; the existence of the
// agent home dirs; and the user-global plugin registry + enable state
// (~/.claude/plugins/installed_plugins.json and the "enabledPlugins" map in
// ~/.claude/settings.json — neither holds secrets). It NEVER reads ~/.claude.json
// (oauth/account/conversation history), and never echoes a raw decoded config map
// into the response — every field below is an explicit typed struct so unknown
// keys are dropped.

// AgentInfo is a coding agent and whether its home dir exists. detail is a fixed
// descriptive string; v1 carries no version.
type AgentInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Detail    string `json:"detail"`
	Connected bool   `json:"connected"`
}

// MCPServerInfo is one entry from the repo .mcp.json mcpServers map. enabled is
// false iff the name appears in .claude/settings.json disabledMcpjsonServers;
// managed is true for the lema server (the one this binary registers).
type MCPServerInfo struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Enabled bool   `json:"enabled"`
	Managed bool   `json:"managed"`
}

// HookInfo is one flattened hook entry from .claude/settings.json. managed is
// true for the lema commit-reminder hook (command contains reminderMarker), the
// only hook this panel can toggle.
type HookInfo struct {
	ID      string `json:"id"`
	Event   string `json:"event"`
	Matcher string `json:"matcher"`
	Label   string `json:"label"`
	Detail  string `json:"detail"`
	Enabled bool   `json:"enabled"`
	Managed bool   `json:"managed"`
}

// ProjectionInfo is a projection file in the served repo root and whether lema
// manages it. present is existence; managed marks an AGENTS.md that carries the
// lema capture block (CLAUDE.md / MEMORY.md are unmanaged in v1).
type ProjectionInfo struct {
	Name    string `json:"name"`
	Present bool   `json:"present"`
	Detail  string `json:"detail"`
	Managed bool   `json:"managed"`
}

// InstalledPlugin is a Claude Code plugin the user has installed (from
// ~/.claude/plugins/installed_plugins.json) with its USER-GLOBAL enabled state
// (the "enabledPlugins" map in ~/.claude/settings.json). provider marks
// lema-shipped plugins (the free carve-out) vs the user's own.
type InstalledPlugin struct {
	Name        string `json:"name"`
	Marketplace string `json:"marketplace"`
	Version     string `json:"version"`
	Enabled     bool   `json:"enabled"`
	Provider    string `json:"provider"` // "lema" | "user"
}

// PluginsSnapshot is the GET /api/plugins (and POST toggle) response: the full
// truthful state of the served repo's agent/plugin/MCP/hook/projection wiring.
type PluginsSnapshot struct {
	Repo        string            `json:"repo"`
	Agents      []AgentInfo       `json:"agents"`
	Plugins     []InstalledPlugin `json:"plugins"`
	MCPServers  []MCPServerInfo   `json:"mcpServers"`
	Hooks       []HookInfo        `json:"hooks"`
	Projections []ProjectionInfo  `json:"projections"`
}

const (
	settingsPath = "./.claude/settings.json"
	mcpJSONPath  = "./.mcp.json"
)

// lemaHookID is the stable id of the lema capture-hook row. The panel ALWAYS
// shows this row (enabled reflecting whether the commit-reminder hook is present
// in settings.json) so the toggle round-trips: disabling removes the hook from
// the file, but the row remains so it can be re-enabled. The user's own hooks are
// listed after it as read-only, position-id'd status rows.
const lemaHookID = "lema-capture"

// pluginsSnapshot builds the panel state from the served repo (the process CWD,
// so files are read at "." and "./.claude/...") plus the existence of the agent
// home dirs. Missing or malformed config is treated as empty so the snapshot is
// always valid (possibly sparse) and never 500s on absent files; the only error
// returned is a genuine read failure of an existing, unreadable file.
func pluginsSnapshot() (PluginsSnapshot, error) {
	snap := PluginsSnapshot{
		Repo:        repoName,
		Agents:      agentInfos(),
		Plugins:     installedPluginInfos(),
		MCPServers:  []MCPServerInfo{},
		Hooks:       []HookInfo{},
		Projections: []ProjectionInfo{},
	}

	settings, err := readJSONObject(settingsPath)
	if err != nil {
		// A malformed settings file must not blank out the whole panel; treat it
		// as empty for read purposes (toggles still re-parse and will surface the
		// parse error on write).
		settings = map[string]any{}
	}
	disabled := disabledServerSet(settings)

	mcp, err := readJSONObject(mcpJSONPath)
	if err != nil {
		mcp = map[string]any{}
	}
	snap.MCPServers = mcpServerInfos(mcp, disabled)
	snap.Hooks = hookInfos(settings)
	snap.Projections, err = projectionInfos()
	if err != nil {
		return PluginsSnapshot{}, err
	}
	return snap, nil
}

// agentInfos reports the fixed set of agents lema interoperates with and whether
// each is installed (its home dir exists under the user's home). Detail strings
// are fixed copy. A missing home dir (or no resolvable home) is simply
// "not connected" — never an error.
func agentInfos() []AgentInfo {
	home, _ := os.UserHomeDir()
	exists := func(name string) bool {
		if home == "" {
			return false
		}
		info, err := os.Stat(filepath.Join(home, name))
		return err == nil && info.IsDir()
	}
	return []AgentInfo{
		{ID: "claude", Name: "Claude Code", Detail: "SDK + hooks + MCP", Connected: exists(".claude")},
		{ID: "codex", Name: "Codex CLI", Detail: "exec --json adapter", Connected: exists(".codex")},
		{ID: "cursor", Name: "Cursor", Detail: "reads workspace via MCP", Connected: exists(".cursor")},
	}
}

// disabledServerSet returns the set of MCP server names disabled for this repo,
// read from settings.json disabledMcpjsonServers (an array of strings). Unknown
// shapes yield an empty set.
func disabledServerSet(settings map[string]any) map[string]bool {
	out := map[string]bool{}
	arr, _ := settings["disabledMcpjsonServers"].([]any)
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out[s] = true
		}
	}
	return out
}

// mcpServerInfos flattens the repo .mcp.json mcpServers map into stable,
// alphabetically-ordered entries. enabled is the inverse of the disabled set;
// managed marks the lema server.
func mcpServerInfos(mcp map[string]any, disabled map[string]bool) []MCPServerInfo {
	servers, _ := mcp["mcpServers"].(map[string]any)
	out := make([]MCPServerInfo, 0, len(servers))
	for _, name := range sortedKeys(servers) {
		entry, _ := servers[name].(map[string]any)
		command, _ := entry["command"].(string)
		out = append(out, MCPServerInfo{
			Name:    name,
			Command: command,
			Enabled: !disabled[name],
			Managed: name == "lema",
		})
	}
	return out
}

// hookInfos returns the capture-hook rows for the panel. The lema-managed
// commit-reminder hook is ALWAYS the first row (id lemaHookID), with enabled set
// to whether it is currently present in settings.json — so the toggle round-trips
// (disabling removes it from the file, but the row stays and can re-enable it).
// The user's own hooks follow as read-only, position-id'd status rows. Claude
// Code has no per-hook disable flag, so user hooks report enabled=true.
func hookInfos(settings map[string]any) []HookInfo {
	hooks, _ := settings["hooks"].(map[string]any)
	lemaPresent := false
	others := []HookInfo{}
	for _, event := range sortedKeys(hooks) {
		groups, _ := hooks[event].([]any)
		idx := 0
		for _, g := range groups {
			group, _ := g.(map[string]any)
			matcher, _ := group["matcher"].(string)
			entries, _ := group["hooks"].([]any)
			for _, h := range entries {
				cmd, _ := h.(map[string]any)
				command, _ := cmd["command"].(string)
				if strings.Contains(command, reminderMarker) {
					lemaPresent = true // shown via the synthesized managed row below
					continue
				}
				others = append(others, HookInfo{
					ID:      strings.ToLower(event) + "-" + itoa(idx),
					Event:   event,
					Matcher: matcher,
					Label:   matcher,
					Detail:  "external hook",
					Enabled: true,
					Managed: false,
				})
				idx++
			}
		}
	}
	managed := HookInfo{
		ID:      lemaHookID,
		Event:   "PostToolUse",
		Matcher: "Bash",
		Label:   "commit reminder",
		Detail:  "record_decision nudge",
		Enabled: lemaPresent,
		Managed: true,
	}
	return append([]HookInfo{managed}, others...)
}

// projectionInfos reports the projection files lema can produce/maintain and
// whether each is present in the served repo root. AGENTS.md is "managed" only
// when it carries the lema capture block; CLAUDE.md and MEMORY.md are unmanaged
// in v1. A read error on an existing file is surfaced; a missing file is simply
// "not present".
func projectionInfos() ([]ProjectionInfo, error) {
	claudePresent, _, err := readProjection("./CLAUDE.md")
	if err != nil {
		return nil, err
	}
	agentsPresent, agentsBody, err := readProjection("./AGENTS.md")
	if err != nil {
		return nil, err
	}
	memoryPresent, _, err := readProjection("./MEMORY.md")
	if err != nil {
		return nil, err
	}
	return []ProjectionInfo{
		{Name: "CLAUDE.md", Present: claudePresent, Detail: "lean · decisions + conventions", Managed: false},
		{Name: "AGENTS.md", Present: agentsPresent, Detail: "capture protocol block", Managed: agentsPresent && strings.Contains(agentsBody, captureBlockStart)},
		{Name: "MEMORY.md", Present: memoryPresent, Detail: "agent memory index", Managed: false},
	}, nil
}

// maxProjectionBytes bounds how much of a projection file is read into memory on
// every GET /api/plugins / toggle. Only the managed-block marker is inspected
// (the body is never echoed), so a file over the cap is reported present with an
// empty body — strings.Contains on "" is false, the conservative managed=false.
// 512 KiB is far above a real CLAUDE.md/AGENTS.md/MEMORY.md yet bounds a
// pathological monolith (ADR-0043/0044).
const maxProjectionBytes = 512 * 1024

// readProjection returns whether path exists and, if so, its contents (capped at
// maxProjectionBytes — an over-cap file is (true, "", nil) so the caller's marker
// check degrades to managed=false rather than loading the whole file). A missing
// file is (false, "", nil); any other read error is returned so the snapshot can
// 500 only on a genuine I/O failure.
func readProjection(path string) (bool, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if info.Size() > maxProjectionBytes {
		return true, "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, "", nil
		}
		return false, "", err
	}
	if len(data) > maxProjectionBytes {
		return true, "", nil
	}
	return true, string(data), nil
}

// httpPlugins (GET /api/plugins) returns the current plugins snapshot for the
// served repo. withToken (serve.go) already guards this route.
func httpPlugins(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET only", http.StatusMethodNotAllowed)
		return
	}
	snap, err := pluginsSnapshot()
	if err != nil {
		http.Error(w, "could not read repo plugin config", http.StatusInternalServerError)
		return
	}
	writeJSONResp(w, snap)
}

// toggleRequest is the POST /api/plugins/toggle body: enable/disable one MCP
// server or the lema-managed hook.
type toggleRequest struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// httpPluginsToggle (POST /api/plugins/toggle) performs one non-destructive
// write — enable/disable an MCP server (via disabledMcpjsonServers, never
// deleting it from .mcp.json) or add/remove the lema commit-reminder hook — then
// returns the fresh snapshot so the UI re-renders truthfully. Bad input is 400;
// a genuine write failure is 500.
func httpPluginsToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in toggleRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	// Reject any name that could escape the repo-local config namespace. MCP
	// server names and hook ids are flat identifiers; a separator or ".." is a
	// path-traversal attempt, never a real value.
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}

	switch in.Kind {
	case "mcp":
		if err := toggleMCP(name, in.Enabled); err != nil {
			http.Error(w, "could not update mcp server", http.StatusInternalServerError)
			return
		}
	case "hook":
		// Only the lema-managed capture hook is toggleable; reject any other id so
		// a bogus name cannot silently 200.
		if name != lemaHookID {
			http.Error(w, "no managed hook with that id", http.StatusBadRequest)
			return
		}
		if err := toggleLemaHook(in.Enabled); err != nil {
			http.Error(w, "could not update hook", http.StatusInternalServerError)
			return
		}
	case "plugin":
		// Plugin enable/disable is USER-GLOBAL (affects every repo), written to
		// ~/.claude/settings.json. Reject a name that is not actually installed so
		// we never write a bogus enabledPlugins key.
		if !isInstalledPlugin(name) {
			http.Error(w, "no installed plugin with that name", http.StatusBadRequest)
			return
		}
		if err := togglePluginEnabled(name, in.Enabled); err != nil {
			http.Error(w, "could not update plugin", http.StatusInternalServerError)
			return
		}
	default:
		http.Error(w, "kind must be mcp, hook, or plugin", http.StatusBadRequest)
		return
	}

	snap, err := pluginsSnapshot()
	if err != nil {
		http.Error(w, "could not read repo plugin config", http.StatusInternalServerError)
		return
	}
	writeJSONResp(w, snap)
}

// toggleMCP enables or disables a repo MCP server without deleting it from
// .mcp.json: disabling adds the name to disabledMcpjsonServers, enabling removes
// it (and, for the lema server only, re-registers it in .mcp.json if it was
// removed entirely). Reuses the init.go JSON helpers.
func toggleMCP(name string, enabled bool) error {
	// Hold the config lock across the whole read-modify-write so concurrent
	// toggles cannot lose-update each other (ADR-0043/0044).
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	if enabled {
		if err := enableMcpServer(settingsPath, name); err != nil {
			return err
		}
		// If the lema server itself was removed from .mcp.json, re-register it so
		// "enable" is meaningful rather than a no-op (ADR-0042 setup).
		if name == "lema" {
			mcp, err := readJSONObject(mcpJSONPath)
			if err != nil {
				return err
			}
			servers, _ := mcp["mcpServers"].(map[string]any)
			if _, ok := servers["lema"]; !ok {
				if _, err := ensureMCPJSON(mcpJSONPath); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return disableMcpServer(settingsPath, name)
}

// toggleLemaHook adds (enabled) or removes (disabled) the lema-managed hooks —
// the commit reminder, the never-reopen guard, and the capture nudge — in the repo
// settings, touching only those managed entries.
func toggleLemaHook(enabled bool) error {
	// Hold the config lock across the read-modify-write (ADR-0043/0044).
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	if enabled {
		if _, err := ensureClaudeHook(settingsPath); err != nil {
			return err
		}
		// The never-reopen guard is a lema-managed hook too (ADR-0052): toggle it
		// on/off in lockstep with the commit reminder.
		if _, err := ensurePreToolUseHook(settingsPath); err != nil {
			return err
		}
		// The capture nudge (ADR-0054) is also lema-managed.
		_, err := ensureCaptureNudgeHook(settingsPath)
		return err
	}
	if _, err := removeLemaHook(settingsPath); err != nil {
		return err
	}
	if _, err := removeGuardHook(settingsPath); err != nil {
		return err
	}
	_, err := removeCaptureNudgeHook(settingsPath)
	return err
}

// userClaudeDir returns the user's ~/.claude directory, or "" if home is
// unresolvable (in which case the plugin features degrade to empty / no-op).
func userClaudeDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// installedPluginInfos lists the Claude Code plugins the user has installed
// (~/.claude/plugins/installed_plugins.json) with their enabled state (the
// "enabledPlugins" map in ~/.claude/settings.json). These are USER-GLOBAL — a
// toggle affects every repo, not just the served one (ADR-0048). Missing files
// yield an empty list; lema-shipped plugins are marked provider="lema".
func installedPluginInfos() []InstalledPlugin {
	dir := userClaudeDir()
	if dir == "" {
		return []InstalledPlugin{}
	}
	enabled := enabledPluginSet(filepath.Join(dir, "settings.json"))
	reg, err := readJSONObject(filepath.Join(dir, "plugins", "installed_plugins.json"))
	if err != nil {
		reg = map[string]any{}
	}
	plugins, _ := reg["plugins"].(map[string]any)
	out := make([]InstalledPlugin, 0, len(plugins))
	for _, name := range sortedKeys(plugins) {
		out = append(out, InstalledPlugin{
			Name:        name,
			Marketplace: marketplaceOf(name),
			Version:     pluginVersion(plugins[name]),
			Enabled:     enabled[name],
			Provider:    pluginProvider(name),
		})
	}
	return out
}

// enabledPluginSet reads the "enabledPlugins" map (name -> bool) from a Claude
// settings file. Absent or non-bool entries are treated as disabled.
func enabledPluginSet(path string) map[string]bool {
	out := map[string]bool{}
	root, err := readJSONObject(path)
	if err != nil {
		return out
	}
	m, _ := root["enabledPlugins"].(map[string]any)
	for k, v := range m {
		if b, ok := v.(bool); ok {
			out[k] = b
		}
	}
	return out
}

// marketplaceOf splits a "plugin@marketplace" id into its marketplace ("" if none).
func marketplaceOf(name string) string {
	if i := strings.LastIndex(name, "@"); i >= 0 {
		return name[i+1:]
	}
	return ""
}

// pluginVersion pulls a version string from a plugin's install-record array,
// preferring a user-scope record; best-effort ("" if unknown).
func pluginVersion(v any) string {
	recs, _ := v.([]any)
	first := ""
	for _, r := range recs {
		m, _ := r.(map[string]any)
		ver, _ := m["version"].(string)
		if scope, _ := m["scope"].(string); scope == "user" && ver != "" {
			return ver
		}
		if first == "" {
			first = ver
		}
	}
	return first
}

// pluginProvider marks lema-shipped plugins (the free carve-out) vs the user's
// own. No lema plugin ships today, so this is forward-looking: a name or
// marketplace that signals lema is "lema", else "user".
func pluginProvider(name string) string {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "lema") || strings.Contains(marketplaceOf(lower), "lema") {
		return "lema"
	}
	return "user"
}

// isInstalledPlugin reports whether name is a currently-installed plugin, so the
// toggle never writes an enabledPlugins key for something that does not exist.
func isInstalledPlugin(name string) bool {
	for _, p := range installedPluginInfos() {
		if p.Name == name {
			return true
		}
	}
	return false
}

// togglePluginEnabled flips enabledPlugins[name] in the user-global
// ~/.claude/settings.json (non-destructive merge — every other setting is
// preserved). The change is GLOBAL: it affects the plugin across all of the
// user's repos (ADR-0048). Serialized under configWriteMu with the atomic writer.
func togglePluginEnabled(name string, enabled bool) error {
	dir := userClaudeDir()
	if dir == "" {
		return fmt.Errorf("cannot resolve home directory")
	}
	path := filepath.Join(dir, "settings.json")
	configWriteMu.Lock()
	defer configWriteMu.Unlock()
	root, err := readJSONObject(path)
	if err != nil {
		return err
	}
	plugins, _ := root["enabledPlugins"].(map[string]any)
	if plugins == nil {
		plugins = map[string]any{}
	}
	plugins[name] = enabled
	root["enabledPlugins"] = plugins
	return writeJSON(path, root)
}

// itoa is a tiny base-10 int formatter (avoids pulling strconv just for hook ids).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// sortedKeys returns the keys of a string-keyed map in ascending order so list
// output (servers, hooks) is stable across calls.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort — these maps are tiny (a handful of servers/events).
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
