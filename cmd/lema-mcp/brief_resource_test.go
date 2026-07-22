package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// lema://brief's contract (decision fa8a63f4): the resource is a THIN WRAPPER
// over the exact code path get_state_brief serves from — one shared helper,
// zero drift. Argumentless by nature, it always takes the prior-run relay
// read; every can't-serve path returns the tool's honest note AS CONTENT,
// never a protocol error; and it registers only in the hosted tier, beside
// the verb it projects.

// TestBriefResourceMatchesToolOutput pins the zero-drift half: for the same
// checkpoint and the same stub server, the resource's text is byte-identical
// to the tool's output. If the resource ever grows its own fetch or render
// logic, this diverges and fails.
func TestBriefResourceMatchesToolOutput(t *testing.T) {
	srv, cap := newBriefTestServer(t, "claude-code", http.StatusOK)
	defer srv.Close()
	setSyncEnv(t, srv.URL)

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv(collectorDirEnv, dir)
	cp := distillEnvelopes([]collectorEnvelope{{
		RunID: "sess-prior", TS: time.Now().UTC().Format(time.RFC3339), Kind: "user_prompt",
		Payload:  map[string]string{"prompt": "resume me"},
		Evidence: map[string]string{"harness": "claude-code", "cwd": cwd},
	}}, cwd)
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	_, toolOut, err := getStateBrief(ctx, nil, stateBriefInput{})
	if err != nil {
		t.Fatal(err)
	}
	if toolOut.Scope != "work unit wu-1" {
		t.Fatalf("tool must have served the stub brief: %+v", toolOut)
	}

	res, err := readBriefResource(ctx, &mcp.ReadResourceRequest{
		Params: &mcp.ReadResourceParams{URI: briefResourceURI},
	})
	if err != nil {
		t.Fatalf("resource read must not be a protocol error: %v", err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("contents = %+v", res.Contents)
	}
	c := res.Contents[0]
	if c.URI != briefResourceURI || c.MIMEType != "application/json" {
		t.Fatalf("contents envelope = %+v", c)
	}
	want, err := json.Marshal(toolOut)
	if err != nil {
		t.Fatal(err)
	}
	if c.Text != string(want) {
		t.Fatalf("resource text drifted from the tool output\nresource: %s\ntool:     %s", c.Text, want)
	}
	// The note attributes the SAME prior-run resolution — the relay read, not
	// some resource-only path.
	if !strings.Contains(c.Text, "sess-prior") {
		t.Fatalf("resource must resolve the prior run like the tool: %s", c.Text)
	}
	if cap.runCreates != 2 {
		t.Fatalf("tool + resource must each ensure the hosted identity, got %d", cap.runCreates)
	}
}

// TestBriefResourceHonestNoteAsContent pins the can't-serve paths: no prior
// run, a dark surface (404), and missing hosted config all come back as the
// tool's honest note in the resource content — never as a read error a host
// would render as failure.
func TestBriefResourceHonestNoteAsContent(t *testing.T) {
	ctx := context.Background()
	req := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: briefResourceURI}}

	// No checkpoint for this project.
	srv, _ := newBriefTestServer(t, "claude-code", http.StatusOK)
	defer srv.Close()
	setSyncEnv(t, srv.URL)
	t.Setenv(collectorDirEnv, t.TempDir())
	res, err := readBriefResource(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, "no prior run known") {
		t.Fatalf("no-checkpoint read must carry the honest note: %+v", res.Contents)
	}

	// Dark surface: a checkpoint exists but the server 404s the brief.
	dark, _ := newBriefTestServer(t, "claude-code", http.StatusNotFound)
	defer dark.Close()
	setSyncEnv(t, dark.URL)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv(collectorDirEnv, dir)
	cp := distillEnvelopes([]collectorEnvelope{{
		RunID: "sess-prior", TS: time.Now().UTC().Format(time.RFC3339), Kind: "user_prompt",
		Payload:  map[string]string{"prompt": "resume me"},
		Evidence: map[string]string{"harness": "claude-code", "cwd": cwd},
	}}, cwd)
	if err := writeCollectorCheckpoint(dir, cp); err != nil {
		t.Fatal(err)
	}
	res, err = readBriefResource(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Contents[0].Text, "unavailable") {
		t.Fatalf("dark surface must be an honest note: %+v", res.Contents)
	}

	// No hosted config at all.
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")
	t.Setenv("LEMA_WORKSPACE_ID", "")
	t.Setenv("HOME", t.TempDir())
	res, err = readBriefResource(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Contents[0].Text, "not configured") {
		t.Fatalf("missing config must be named: %+v", res.Contents)
	}
}

// TestBriefResourceRegistration pins what registerBriefResource exposes — the
// single static lema://brief, readable over a real session — and that a server
// WITHOUT it lists no resources at all. main.go calls registerBriefResource
// only inside the hosted (LEMA_API_URL) block, mirroring get_state_brief's
// tier gate; this test covers the registration function directly because that
// gate is an inline branch of main.
func TestBriefResourceRegistration(t *testing.T) {
	ctx := context.Background()

	connect := func(t *testing.T, server *mcp.Server) *mcp.ClientSession {
		t.Helper()
		clientT, serverT := mcp.NewInMemoryTransports()
		ss, err := server.Connect(ctx, serverT, nil)
		if err != nil {
			t.Fatalf("server.Connect: %v", err)
		}
		t.Cleanup(func() { _ = ss.Close() })
		client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
		cs, err := client.Connect(ctx, clientT, nil)
		if err != nil {
			t.Fatalf("client.Connect: %v", err)
		}
		t.Cleanup(func() { _ = cs.Close() })
		return cs
	}

	// The non-hosted shape: no registration, no resources.
	bare := mcp.NewServer(&mcp.Implementation{Name: "lema-mcp", Version: "test"}, nil)
	bareList, err := connect(t, bare).ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(bareList.Resources) != 0 {
		t.Fatalf("an unregistered server must expose no resources: %+v", bareList.Resources)
	}

	// The hosted shape: exactly lema://brief, with its directory metadata.
	server := mcp.NewServer(&mcp.Implementation{Name: "lema-mcp", Version: "test"}, nil)
	registerBriefResource(server)
	cs := connect(t, server)
	list, err := cs.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(list.Resources) != 1 {
		t.Fatalf("resources/list = %+v, want exactly lema://brief (fa8a63f4 ruled out further resources)", list.Resources)
	}
	r := list.Resources[0]
	if r.URI != briefResourceURI || r.Name != "state-brief" || r.MIMEType != "application/json" {
		t.Fatalf("resource = %+v", r)
	}
	if r.Title == "" || r.Description == "" {
		t.Fatalf("resource must carry host-facing title + description: %+v", r)
	}

	// And it reads over the wire: hermetic env (no hosted config) so the
	// content is the honest not-configured note.
	t.Setenv("LEMA_API_URL", "")
	t.Setenv("LEMA_API_TOKEN", "")
	t.Setenv("LEMA_WORKSPACE_ID", "")
	t.Setenv("HOME", t.TempDir())
	read, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: briefResourceURI})
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(read.Contents) != 1 || !strings.Contains(read.Contents[0].Text, "not configured") {
		t.Fatalf("wire read must serve the honest note as content: %+v", read.Contents)
	}
}
