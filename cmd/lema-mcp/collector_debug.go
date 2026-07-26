// collector_debug.go is the opt-in breadcrumb for the two collector paths
// that carry the relay: the boundary sync (collector_sync.go) and the
// SessionStart injection (collector_checkpoint.go).
//
// Both are fail-open by design — a hook is never blocked and the local spool
// stays authoritative — which means every failure is a silent no-op and the
// operator cannot tell missing credentials from a dark-flag 404 from a run
// identity mismatch from a timeout. Worse, when the hosted State Brief cannot
// be read the injection degrades to the pre-0.21.4 local block, which looks
// plausible and reports nothing.
//
// This changes no control flow. It only names the cause on the way out, and
// only when asked.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// collectorDebugEnv opts a session into the breadcrumb. Any non-empty value
// enables it; it is off by default because these paths run inside hook budgets
// on every session boundary.
const collectorDebugEnv = "LEMA_COLLECTOR_DEBUG"

// collectorDebugOut is the diagnostic sink and a test seam. It is stderr and
// never stdout: stdout carries the SessionStart additionalContext JSON, so a
// diagnostic byte written there corrupts the injection this exists to debug.
var collectorDebugOut io.Writer = os.Stderr

// collectorDebugf reports why a collector path did what it did. It never
// returns an error and never blocks: a diagnostic that can fail a hook is
// worse than no diagnostic.
func collectorDebugf(format string, args ...any) {
	if strings.TrimSpace(os.Getenv(collectorDebugEnv)) == "" {
		return
	}
	fmt.Fprintf(collectorDebugOut, "lema-mcp collector: "+format+"\n", args...)
}
