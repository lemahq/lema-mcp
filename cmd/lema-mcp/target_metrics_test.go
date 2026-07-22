package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func installTargetMetricLog(t *testing.T) *os.File {
	t.Helper()
	oldLog, oldVersion := usageLog, Version
	file, err := os.CreateTemp(t.TempDir(), "target-metrics-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	usageLog = file
	Version = "6.1.0-test"
	t.Cleanup(func() {
		usageLog = oldLog
		Version = oldVersion
		_ = file.Close()
	})
	return file
}

func installSynchronousTargetShadow(t *testing.T) {
	t.Helper()
	old := launchTargetShadow
	launchTargetShadow = func(parent context.Context, observe func(context.Context)) {
		child, cancel := context.WithCancel(parent)
		defer cancel()
		observe(child)
	}
	t.Cleanup(func() { launchTargetShadow = old })
}

func readTargetMetrics(t *testing.T, file *os.File) []map[string]string {
	t.Helper()
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var record map[string]string
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("metric line is not JSON: %v\n%s", err, line)
		}
		records = append(records, record)
	}
	return records
}

func TestTargetMetricUsesOnlySafeContractFields(t *testing.T) {
	file := installTargetMetricLog(t)
	writeTargetMetric(targetMetric{
		PackageVersion:       Version,
		ResolutionStatus:     resolutionResolved,
		ResolutionRung:       "canonical_git",
		ProjectResourceID:    targetMetricResourceID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
		RepositoryResourceID: targetMetricResourceID("private-repository-slug"),
		MismatchCategory:     targetMismatchRepository,
	})

	records := readTargetMetrics(t, file)
	if len(records) != 1 {
		t.Fatalf("metric records = %d, want 1", len(records))
	}
	record := records[0]
	wantKeys := []string{"mismatch_category", "package_version", "project_resource_id", "repository_resource_id", "resolution_rung", "resolution_status"}
	gotKeys := make([]string, 0, len(record))
	for key := range record {
		gotKeys = append(gotKeys, key)
	}
	slicesSort(gotKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("metric keys = %v, want privacy contract %v", gotKeys, wantKeys)
	}
	if record["project_resource_id"] != "uuid:…aaaaaaaa" {
		t.Fatalf("UUID metric id = %q, want validated suffix", record["project_resource_id"])
	}
	if !strings.HasPrefix(record["repository_resource_id"], "sha256:") || strings.Contains(record["repository_resource_id"], "slug") {
		t.Fatalf("non-UUID metric id = %q, want stable hash", record["repository_resource_id"])
	}
	for _, forbidden := range []string{"token", "path", "remote", "prompt", "query", "decision", "private-repository-slug"} {
		if strings.Contains(string(mustJSON(t, record)), forbidden) {
			t.Fatalf("metric leaked forbidden field/content %q: %#v", forbidden, record)
		}
	}
}

func slicesSort(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type compatibilityTargetProvider struct {
	primary resolutionResult
	shadow  resolutionResult
	err     error
	calls   int
}

func (p *compatibilityTargetProvider) Resolve(_ context.Context, in resolveTargetInput) (resolutionResult, error) {
	p.calls++
	if hasExplicitTarget(in) {
		return p.primary, p.err
	}
	return p.shadow, nil
}

func metricContext(projectID, repositoryID, rung string) targetContext {
	receipt := validRoutingContext()
	receipt.ProjectWorkspaceID = projectID
	receipt.RepositoryWorkspaceID = repositoryID
	receipt.VisibleRepositoryWorkspaceIDs = []string{repositoryID}
	receipt.ResolvedBy = rung
	return receipt
}

func TestObservedTargetProviderShadowsLegacyWithoutChangingItsOperationTarget(t *testing.T) {
	file := installTargetMetricLog(t)
	installSynchronousTargetShadow(t)
	base := &compatibilityTargetProvider{
		primary: resolutionResult{Status: resolutionResolved, Context: metricContext(
			"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			"explicit",
		)},
		shadow: resolutionResult{Status: resolutionResolved, Context: metricContext(
			"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			"cccccccc-cccc-cccc-cccc-cccccccccccc",
			"canonical_git",
		)},
	}
	provider := newObservedTargetProvider(base)
	var operations int
	got, err := withResolvedTarget(context.Background(), provider, resolveTargetInput{
		ExplicitWorkspaceID: "legacy-private-slug",
		CWD:                 "/private/operator/repository",
	}, func(_ context.Context, receipt targetContext) (string, error) {
		operations++
		return receipt.RepositoryWorkspaceID, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" {
		t.Fatalf("operation target = %q, want authoritative legacy repository", got)
	}
	if base.calls != 2 || operations != 1 {
		t.Fatalf("resolver calls=%d operations=%d, want compatibility lookup 2 and operation 1", base.calls, operations)
	}

	records := readTargetMetrics(t, file)
	if len(records) != 2 {
		t.Fatalf("metric records = %#v, want legacy-use and comparison records", records)
	}
	if records[0]["mismatch_category"] != targetMismatchLegacyUse || records[0]["resolution_rung"] != "explicit" {
		t.Fatalf("primary metric = %#v, want separately countable legacy use", records[0])
	}
	if records[1]["mismatch_category"] != targetMismatchRepository || records[1]["resolution_rung"] != "canonical_git" {
		t.Fatalf("shadow metric = %#v, want repository mismatch on new resolution rung", records[1])
	}
	joined := string(mustJSON(t, records))
	for _, forbidden := range []string{"legacy-private-slug", "/private/operator/repository", "git:example.test/acme/api"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("compatibility metric leaked %q: %s", forbidden, joined)
		}
	}
}

func TestObservedTargetProviderNeverBroadensFailedExplicitResolution(t *testing.T) {
	file := installTargetMetricLog(t)
	installSynchronousTargetShadow(t)
	base := &compatibilityTargetProvider{
		primary: resolutionResult{Status: resolutionStale, Reason: "secret path and token"},
		shadow:  resolutionResult{Status: resolutionResolved, Context: metricContext("project-shadow", "repo-shadow", "canonical_git")},
	}
	provider := newObservedTargetProvider(base)
	var operations int
	_, err := withResolvedTarget(context.Background(), provider, resolveTargetInput{
		ExplicitWorkspaceID: "removed-private-target",
		CWD:                 "/private/operator/repository",
	}, func(_ context.Context, _ targetContext) (struct{}, error) {
		operations++
		return struct{}{}, nil
	})
	if targetResolutionStatusFromError(err) != resolutionStale {
		t.Fatalf("result error = %v, want authoritative stale", err)
	}
	if base.calls != 2 || operations != 0 {
		t.Fatalf("resolver calls=%d operations=%d, want compatibility lookup 2 and operation 0", base.calls, operations)
	}
	records := readTargetMetrics(t, file)
	if len(records) != 2 || records[0]["resolution_status"] != "stale" || records[0]["resolution_rung"] != "explicit" || records[0]["mismatch_category"] != targetMismatchLegacyUse || records[1]["mismatch_category"] != targetMismatchStatus {
		t.Fatalf("metrics = %#v, want separate stale, legacy-use, and status-mismatch counts", records)
	}
	if strings.Contains(string(mustJSON(t, records)), "secret") || strings.Contains(string(mustJSON(t, records)), "private") {
		t.Fatalf("failed resolution leaked reason or input: %#v", records)
	}
}

type blockingShadowProvider struct {
	primary        resolutionResult
	shadowStarted  chan struct{}
	releaseShadow  chan struct{}
	shadowFinished chan struct{}
}

func (p *blockingShadowProvider) Resolve(ctx context.Context, in resolveTargetInput) (resolutionResult, error) {
	if hasExplicitTarget(in) {
		return p.primary, nil
	}
	close(p.shadowStarted)
	select {
	case <-p.releaseShadow:
	case <-ctx.Done():
	}
	close(p.shadowFinished)
	return resolutionResult{Status: resolutionUnresolved}, nil
}

func TestObservedTargetProviderBlockedShadowCannotDelayOperation(t *testing.T) {
	file := installTargetMetricLog(t)
	base := &blockingShadowProvider{
		primary: resolutionResult{Status: resolutionResolved, Context: metricContext(
			"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			"explicit",
		)},
		shadowStarted:  make(chan struct{}),
		releaseShadow:  make(chan struct{}),
		shadowFinished: make(chan struct{}),
	}
	provider := newObservedTargetProvider(base)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var operations int
	got, err := withResolvedTarget(ctx, provider, resolveTargetInput{
		ExplicitWorkspaceID: "legacy-private-slug",
		CWD:                 "/private/operator/repository",
	}, func(operationCtx context.Context, receipt targetContext) (string, error) {
		operations++
		<-base.shadowStarted
		if operationCtx.Err() != nil {
			t.Fatalf("operation callback received consumed context: %v", operationCtx.Err())
		}
		select {
		case <-base.shadowFinished:
			t.Fatal("operation callback waited for the blocked compatibility shadow")
		default:
		}
		return receipt.RepositoryWorkspaceID, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" || operations != 1 {
		t.Fatalf("operation result=%q calls=%d, want authoritative primary exactly once", got, operations)
	}
	close(base.releaseShadow)
	<-base.shadowFinished

	records := readTargetMetrics(t, file)
	if len(records) != 2 || records[0]["mismatch_category"] != targetMismatchLegacyUse || records[1]["mismatch_category"] != targetMismatchStatus {
		t.Fatalf("metrics after released shadow = %#v", records)
	}
}

func TestObservedTargetProviderPinsMetricSinkBeforeBackgroundShadow(t *testing.T) {
	oldLog, oldVersion := usageLog, Version
	first, err := os.CreateTemp(t.TempDir(), "first-target-metrics-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.CreateTemp(t.TempDir(), "second-target-metrics-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	usageLog, Version = first, "first-version"
	t.Cleanup(func() {
		usageLog, Version = oldLog, oldVersion
		_ = first.Close()
		_ = second.Close()
	})

	base := &blockingShadowProvider{
		primary: resolutionResult{Status: resolutionResolved, Context: metricContext(
			"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			"explicit",
		)},
		shadowStarted:  make(chan struct{}),
		releaseShadow:  make(chan struct{}),
		shadowFinished: make(chan struct{}),
	}
	provider := newObservedTargetProvider(base)
	_, err = withResolvedTarget(context.Background(), provider, resolveTargetInput{ExplicitWorkspaceID: "legacy"}, func(context.Context, targetContext) (struct{}, error) {
		<-base.shadowStarted
		return struct{}{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a later process/test changing the global destination while the
	// first provider's compatibility observation is still in flight.
	usageLog, Version = second, "second-version"
	close(base.releaseShadow)
	<-base.shadowFinished

	firstRecords := readTargetMetrics(t, first)
	if len(firstRecords) != 2 {
		t.Fatalf("original sink records = %#v, want primary and shadow", firstRecords)
	}
	for _, record := range firstRecords {
		if record["package_version"] != "first-version" {
			t.Fatalf("background metric version = %q, want provider's pinned version", record["package_version"])
		}
	}
	if secondRecords := readTargetMetrics(t, second); len(secondRecords) != 0 {
		t.Fatalf("background shadow crossed into later metric sink: %#v", secondRecords)
	}
}

func TestBoundedTargetShadowHonorsParentCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	finished := make(chan error, 1)
	launchBoundedTargetShadow(parent, time.Hour, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		finished <- ctx.Err()
	})
	<-started
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("shadow context error = %v, want parent cancellation", err)
	}
}

func TestBoundedTargetShadowCannotLeakPastTimeout(t *testing.T) {
	finished := make(chan error, 1)
	launchBoundedTargetShadow(context.Background(), 0, func(ctx context.Context) {
		<-ctx.Done()
		finished <- ctx.Err()
	})
	if err := <-finished; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shadow context error = %v, want bounded deadline", err)
	}
}

func TestObservedTargetProviderCountsEveryPrimaryResolutionStatus(t *testing.T) {
	for _, status := range []resolutionStatus{resolutionUnresolved, resolutionAmbiguous, resolutionStale} {
		t.Run(string(status), func(t *testing.T) {
			file := installTargetMetricLog(t)
			base := &compatibilityTargetProvider{shadow: resolutionResult{Status: status, Reason: "private detail"}}
			provider := newObservedTargetProvider(base)
			_, _ = provider.Resolve(context.Background(), resolveTargetInput{CWD: "/private/path"})
			records := readTargetMetrics(t, file)
			if len(records) != 1 || records[0]["resolution_status"] != string(status) || records[0]["mismatch_category"] != "" {
				t.Fatalf("metrics = %#v, want one primary %s count", records, status)
			}
			if base.calls != 1 {
				t.Fatalf("resolver calls = %d, want no shadow without explicit target", base.calls)
			}
		})
	}
}

type driftingBriefOutput struct {
	Sections []int `json:"sections"`
}

func (driftingBriefOutput) MarshalJSON() ([]byte, error) {
	return []byte(`{"sections":[{"name":"objective","text":"private decision content"}]}`), nil
}

func TestStateBriefSchemaFailureMetricObservesSDKOutputValidationBoundary(t *testing.T) {
	file := installTargetMetricLog(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "lema-mcp", Version: "test"}, nil)
	server.AddReceivingMiddleware(stateBriefSchemaMetricMiddleware)
	mcp.AddTool(server, &mcp.Tool{Name: "get_state_brief"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, driftingBriefOutput, error) {
		return nil, driftingBriefOutput{}, nil
	})

	ctx := context.Background()
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_state_brief"})
	if err == nil || !strings.Contains(err.Error(), "validating tool output") {
		t.Fatalf("SDK call error = %v, want reproduced output validation failure", err)
	}
	records := readTargetMetrics(t, file)
	if len(records) != 1 || records[0]["mismatch_category"] != targetMismatchSchemaFailure || records[0]["resolution_rung"] != "state_brief_output" {
		t.Fatalf("schema metrics = %#v, want one separately countable schema failure", records)
	}
	encoded := string(mustJSON(t, records))
	for _, forbidden := range []string{"private decision content", "validating", "sections", "items"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("schema metric leaked validation payload %q: %s", forbidden, encoded)
		}
	}
}

func TestStateBriefSchemaMetricMiddlewareIgnoresUnrelatedFailures(t *testing.T) {
	file := installTargetMetricLog(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "lema-mcp", Version: "test"}, nil)
	server.AddReceivingMiddleware(stateBriefSchemaMetricMiddleware)
	mcp.AddTool(server, &mcp.Tool{Name: "another_tool"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, errors.New("validating tool output: private payload")
	})

	ctx := context.Background()
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	_, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "another_tool"})
	if records := readTargetMetrics(t, file); len(records) != 0 {
		t.Fatalf("unrelated failure emitted State Brief schema metric: %#v", records)
	}
}
