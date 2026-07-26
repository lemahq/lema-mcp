package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lemahq/lema-mcp/internal/source"
	"github.com/lemahq/lema-mcp/internal/verdict"
)

// kafkaQueueRecord is the shared message-queue fixture: both guard test files
// record it, so the killed option and its recorded why stay single-sourced.
var kafkaQueueRecord = source.DecisionRecord{
	Title: "message queue", Chosen: "NATS",
	Rejected: []source.RejectedAlt{{Option: "Kafka", Why: "operational burden for our scale"}},
}

// storeWith records the given decisions into a fresh capture store.
func storeWith(t *testing.T, recs ...source.DecisionRecord) *source.CaptureStore {
	t.Helper()
	s, err := source.NewCaptureStore(filepath.Join(t.TempDir(), "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if _, err := s.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func newTestStore(t *testing.T) *source.CaptureStore {
	t.Helper()
	return storeWith(t, kafkaQueueRecord, source.DecisionRecord{
		Title: "auth", Chosen: "sessions",
		Rejected: []source.RejectedAlt{{Option: "JWT", Why: "hard to revoke"}},
	})
}

func ctxQuery(file, newStr string) string {
	return guardQuery(map[string]any{"file_path": file, "new_string": newStr})
}

func TestGuardQuery(t *testing.T) {
	q := guardQuery(map[string]any{
		"file_path":  "internal/queue/kafka.go",
		"old_string": "REMOVED_TOKEN_OLDSTUFF",
		"new_string": "connect to Kafka",
		"content":    "writeBody",
		"command":    "npm install kafkajs",
		"edits":      []any{map[string]any{"new_string": "import mongo"}},
	})
	for _, want := range []string{"kafka.go", "connect to Kafka", "writeBody", "kafkajs", "import mongo"} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q: %q", want, q)
		}
	}
	// old_string (the REMOVED text) is not signal — you're deleting it, not proposing it.
	if strings.Contains(q, "REMOVED_TOKEN_OLDSTUFF") {
		t.Errorf("old_string should be excluded: %q", q)
	}
	// 'description' is not a field real Claude Code Edits emit — it must be ignored.
	if d := guardQuery(map[string]any{"description": "switch to Kafka"}); d != "" {
		t.Errorf("a 'description' field must be ignored, got %q", d)
	}
}

func TestTokenizeSplitsIdentifiers(t *testing.T) {
	got := strings.Join(verdict.Tokenize("kafka.NewProducer() + KafkaBrokers"), ",")
	for _, w := range []string{"kafka", "new", "producer", "brokers"} {
		if !strings.Contains(got, w) {
			t.Errorf("tokenize missing %q in %q", w, got)
		}
	}
}

func TestOptionMatches(t *testing.T) {
	cases := []struct {
		key, edit string
		want      bool
	}{
		{"Kafka", "kafka.NewProducer()", true},                    // dotted identifier
		{"Kafka", "NewKafkaClient()", true},                       // camelCase
		{"Kafka", `import "github.com/segmentio/kafka-go"`, true}, // import path
		{"Kafka", "this is a kafkaesque mess", false},             // substring, not a whole token
		{"Kafka", "reduce operational burden", false},             // rationale prose, not the option
		{"Spring Boot", "use spring-boot here", true},
		{"Spring Boot", "spring is in the air", false}, // missing 'boot'
		{"PostgreSQL", "we use postgresql now", true},  // joined form
		{"PostgreSQL", "we use PostgreSQL now", true},  // camelCase pieces
		{"Go", "let it go now", false},                 // too short (<3)

		// Adjacency (pain-point #27): a multi-word option must be NAMED, not
		// merely have its vocabulary scattered through the text. Without this,
		// any long document containing the ordinary words of an option fired it.
		{"Spring Boot", "spring cleaning, then reboot the boot loader", false},
		{"Keep \"lema Workspaces\"", "we keep the lema flag as-is; see /workspaces/{id}/runs", false},
		{"Build it on the lema (server) side", "build the read path first; on the lema side that is a sibling", false},
		{"Spring Boot", "migrate to Spring Boot 3", true}, // still fires when actually named
	}
	for _, c := range cases {
		got, _ := optionMatches(c.key, newGuardText(c.edit))
		if got != c.want {
			t.Errorf("optionMatches(%q, %q) = %v, want %v", c.key, c.edit, got, c.want)
		}
	}
}

func TestEvaluateGuard(t *testing.T) {
	store := newTestStore(t)

	// Killed option reached via a real code identifier, context mode → a
	// non-blocking nudge with NO permissionDecision (never skips the user's prompt).
	out, atom := evaluateGuard(store.ClosedAtoms(), ctxQuery("queue.go", "kafka.NewProducer()"), "queue.go", guardModeContext)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("context mode must nudge with no permissionDecision, got %+v", out)
	}
	if atom == nil || !strings.Contains(out.HookSpecificOutput.AdditionalContext, "Kafka") {
		t.Fatalf("expected a Kafka nudge + matched atom, got %+v / %+v", out, atom)
	}

	// Rationale prose alone must NOT fire (names the Why, not the option).
	if out, _ := evaluateGuard(store.ClosedAtoms(), ctxQuery("x.go", "reduce operational burden across the system"), "x.go", guardModeContext); out != nil {
		t.Fatalf("rationale prose must not fire, got %+v", out)
	}
	// Substring of a larger word must NOT fire.
	if out, _ := evaluateGuard(store.ClosedAtoms(), ctxQuery("x.go", "a very kafkaesque situation"), "x.go", guardModeContext); out != nil {
		t.Fatalf("substring must not fire, got %+v", out)
	}

	// Ask mode: a specific option (Kafka, score 5) prompts the human.
	out, _ = evaluateGuard(store.ClosedAtoms(), ctxQuery("q.go", "wire up Kafka consumer"), "q.go", guardModeAsk)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "ask" {
		t.Fatalf("strong hit in ask mode must prompt, got %+v", out)
	}
	// Ask mode: a weak/short option (JWT, score 3 < ask floor 5) degrades to a nudge.
	out, _ = evaluateGuard(store.ClosedAtoms(), ctxQuery("auth.go", "use JWT here"), "auth.go", guardModeAsk)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "" {
		t.Fatalf("weak hit in ask mode must degrade to a nudge, got %+v", out)
	}

	// Off mode → nothing, even on a hit.
	if out, _ := evaluateGuard(store.ClosedAtoms(), "use Kafka", "", guardModeOff); out != nil {
		t.Fatalf("off mode must emit nothing, got %+v", out)
	}

	// The shipped JSON must NOT carry permissionDecision in context mode.
	out, _ = evaluateGuard(store.ClosedAtoms(), ctxQuery("q.go", "Kafka here"), "q.go", guardModeContext)
	if b, _ := json.Marshal(out); strings.Contains(string(b), "permissionDecision") {
		t.Fatalf("context-mode JSON must omit permissionDecision: %s", b)
	}
}

func TestRunGuardStdin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.jsonl")
	s, _ := source.NewCaptureStore(path)
	if _, err := s.Record(source.DecisionRecord{Title: "queue", Chosen: "NATS",
		Rejected: []source.RejectedAlt{{Option: "Kafka", Why: "ops burden"}}}); err != nil {
		t.Fatal(err)
	}
	// Real Edit schema: file_path, old_string, new_string.
	stdin := `{"tool_name":"Edit","tool_input":{"file_path":"q.go","old_string":"x","new_string":"new KafkaClient()"}}`
	out := captureRunGuard(t, stdin, []string{"--capture-file", path})
	if !strings.Contains(out, "additionalContext") || !strings.Contains(out, "Kafka") {
		t.Fatalf("expected a Kafka context nudge, got: %q", out)
	}
	if strings.Contains(out, "permissionDecision") {
		t.Fatalf("context mode must not emit permissionDecision: %q", out)
	}
	// Malformed stdin → fail-open: nothing.
	if got := captureRunGuard(t, "not json", []string{"--capture-file", path}); strings.TrimSpace(got) != "" {
		t.Fatalf("malformed input should emit nothing, got: %q", got)
	}
}

// captureRunGuard runs runGuard with os.Stdin/os.Stdout redirected, returning stdout.
func captureRunGuard(t *testing.T, stdin string, args []string) string {
	t.Helper()
	rIn, wIn, _ := os.Pipe()
	if _, err := wIn.WriteString(stdin); err != nil {
		t.Fatal(err)
	}
	wIn.Close()
	rOut, wOut, _ := os.Pipe()
	origIn, origOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = rIn, wOut
	defer func() { os.Stdin, os.Stdout = origIn, origOut }()
	runGuard(args, nil)
	wOut.Close()
	b, _ := io.ReadAll(rOut)
	return string(b)
}

func TestGuardLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "nested", "dir", "guard.log") // also tests MkdirAll
	t.Setenv("LEMA_GUARD_LOG", logPath)
	t.Setenv("LEMA_GUARD_MODE", "context")
	out := &guardOutput{HookSpecificOutput: hookSpecificOutput{AdditionalContext: "Kafka"}}
	atom := &source.Atom{Ref: "d_abc123", Score: 5, MatchKey: "Kafka"}
	guardLog(guardInput{ToolName: "Edit"}, out, "queue.go new KafkaClient", atom)

	b, _ := os.ReadFile(logPath)
	s := string(b)
	for _, want := range []string{`"tool":"Edit"`, `"decision":"context"`, `"score":5`, "d_abc123", `"query":"queue.go new KafkaClient"`} {
		if !strings.Contains(s, want) {
			t.Errorf("guard log missing %q: %s", want, s)
		}
	}

	// Unset → silent no-op (no panic, nothing written).
	t.Setenv("LEMA_GUARD_LOG", "")
	guardLog(guardInput{ToolName: "Write"}, &guardOutput{}, "q", nil)
}
