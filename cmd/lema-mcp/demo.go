package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/lemahq/lema-mcp/internal/source"
)

// runDemo shows the never-reopen guarantee in ~30 seconds using the REAL capture
// store + enforcement (not a canned string), against a scratch file that is
// deleted afterward — nothing touches the user's repo. It is the standalone's
// instant hook and the landing's hero CTA (`npx lema-mcp demo`).
func runDemo(_ []string) error {
	return demoTo(os.Stdout)
}

func demoTo(w io.Writer) error {
	dir, err := os.MkdirTemp("", "lema-demo-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	store, err := source.NewCaptureStore(filepath.Join(dir, "decisions.jsonl"))
	if err != nil {
		return err
	}

	// 1. The agent settles a decision — through the real write path.
	rec, err := store.Record(source.DecisionRecord{
		Title:     "Data fetching for the web app",
		Chosen:    "TanStack Query",
		Rejected:  []source.RejectedAlt{{Option: "SWR", Why: "no first-class mutation / cache invalidation — we'd hand-roll it"}},
		Rationale: "server-state caching with real mutations matches our fetch shape",
	})
	if err != nil {
		return err
	}

	// 2. A later session reaches for the rejected option — the real read path.
	closedNote := "do not propose \"SWR\" — it was ruled out"
	for _, a := range store.CheckDecided("use SWR for data fetching", 5) {
		if a.Type == "rejected_alternative" {
			closedNote = a.ClosedNote
			break
		}
	}

	p := func(s string) { fmt.Fprintln(w, s) }
	p("")
	p("  lema never-reopen — a 30-second walkthrough (nothing is written to your repo)")
	p("")
	p("  1. Your agent settles a decision while working:")
	fmt.Fprintf(w, "       record_decision( chose: %q,\n", rec.Chosen)
	fmt.Fprintf(w, "                         rejected: %q — %s )\n", rec.Rejected[0].Option, rec.Rejected[0].Why)
	p("     ✓ recorded.")
	p("")
	p("  2. Later — new session, new task — your agent reaches for SWR:")
	p("       check_decided(\"use SWR for data fetching\")")
	p("")
	fmt.Fprintf(w, "       ⛔ CLOSED — %s\n", closedNote)
	p("")
	p("  3. So instead of re-proposing a dead end, your agent surfaces the decision")
	p("     you already made — and moves on. That's never-reopen.")
	p("")
	p("  Wire it into your agent:  npx lema-mcp init        See the vision:  lema.sh")
	p("")
	return nil
}
