package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Per-user credentials file (ADR-0060, resolved question 1): the token
// delivery channel for GUI-launched editors whose MCP child doesn't inherit
// shell env. Format is a minimal KEY=VALUE file:
//
//	LEMA_API_URL=https://api.lema.sh
//	LEMA_API_TOKEN=lema_live_...
//
// Environment variables always win; the file only fills what env leaves
// unset, so existing shell-env setups behave exactly as before. The file
// must never live in a repo — it is per-user, chmod 600.

const credentialsRelPath = ".config/lema/credentials"

// credentialsPath returns ~/.config/lema/credentials, or "" when no home
// directory is resolvable.
func credentialsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, credentialsRelPath)
}

// readCredentialsFile parses the per-user credentials file. A missing file
// is (nil, nil) — the normal case for env-configured or local-only use.
// A present-but-loose file (group/world readable) still loads, with a
// warning: refusing outright would brick a working setup over an fixable
// chmod, but the user must hear about it.
func readCredentialsFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "lema-mcp: WARNING: %s is readable by other users — run: chmod 600 %s\n", path, path)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, sc.Err()
}

// resolveHostedConfig merges the hosted-mode configuration: env first, then
// the per-user credentials file for whatever env leaves unset. usedFile
// reports whether the file contributed anything, for the startup log line.
func resolveHostedConfig() (apiURL, token string, usedFile bool) {
	apiURL = os.Getenv("LEMA_API_URL")
	token = os.Getenv("LEMA_API_TOKEN")
	if apiURL != "" && token != "" {
		return apiURL, token, false
	}
	creds, err := readCredentialsFile(credentialsPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "lema-mcp: could not read credentials file: %v\n", err)
		return apiURL, token, false
	}
	if creds == nil {
		return apiURL, token, false
	}
	if apiURL == "" && creds["LEMA_API_URL"] != "" {
		apiURL = creds["LEMA_API_URL"]
		usedFile = true
	}
	if token == "" && creds["LEMA_API_TOKEN"] != "" {
		token = creds["LEMA_API_TOKEN"]
		usedFile = true
	}
	return apiURL, token, usedFile
}
