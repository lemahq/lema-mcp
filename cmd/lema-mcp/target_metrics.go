package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	targetMismatchLegacyUse     = "legacy_explicit_use"
	targetMismatchMatch         = "legacy_shadow_match"
	targetMismatchStatus        = "cross_operation_status"
	targetMismatchProject       = "cross_operation_project"
	targetMismatchRepository    = "cross_operation_repository"
	targetMismatchSchemaFailure = "schema_failure"
	// Git evidence has its own two-second command budget and Project discovery
	// may issue sequential link reads. Five seconds bounds a stuck observation
	// without systematically timing out healthy larger Organizations.
	targetShadowTimeout = 5 * time.Second
)

type targetShadowLauncher func(context.Context, func(context.Context))

var launchTargetShadow targetShadowLauncher = func(parent context.Context, observe func(context.Context)) {
	launchBoundedTargetShadow(parent, targetShadowTimeout, observe)
}

// launchBoundedTargetShadow starts compatibility-only work without making the
// primary operation wait for it. The child keeps parent cancellation and also
// owns a short deadline so a stuck resolver cannot leak a goroutine indefinitely.
func launchBoundedTargetShadow(parent context.Context, timeout time.Duration, observe func(context.Context)) {
	if observe == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	go func() {
		defer cancel()
		observe(ctx)
	}()
}

// targetMetric is the complete local compatibility-observation contract. Keep
// this deliberately smaller than the general usage log: target observations
// must never acquire queries, paths, remotes, credentials, or response content.
type targetMetric struct {
	PackageVersion       string           `json:"package_version"`
	ResolutionStatus     resolutionStatus `json:"resolution_status,omitempty"`
	ResolutionRung       string           `json:"resolution_rung,omitempty"`
	ProjectResourceID    string           `json:"project_resource_id,omitempty"`
	RepositoryResourceID string           `json:"repository_resource_id,omitempty"`
	MismatchCategory     string           `json:"mismatch_category,omitempty"`
}

func writeTargetMetric(metric targetMetric) {
	captureTargetMetricSink().write(metric)
}

// targetMetricSink is captured with an observed provider so asynchronous
// compatibility work cannot follow a later global log-file change or version
// override. Production config is process-stable; pinning also makes that
// ownership explicit and prevents late writes from crossing destinations.
type targetMetricSink struct {
	packageVersion string
	usageLog       *os.File
}

func captureTargetMetricSink() targetMetricSink {
	logMu.Lock()
	defer logMu.Unlock()
	return targetMetricSink{packageVersion: Version, usageLog: usageLog}
}

func (sink targetMetricSink) write(metric targetMetric) {
	metric.PackageVersion = sink.packageVersion
	line, err := json.Marshal(metric)
	if err != nil {
		return
	}
	fmt.Fprintln(os.Stderr, "lema-mcp target_metric "+string(line))
	if sink.usageLog != nil {
		logMu.Lock()
		fmt.Fprintln(sink.usageLog, string(line))
		logMu.Unlock()
	}
}

// targetMetricResourceID uses a human-auditable suffix only for a syntactically
// valid UUID. Slugs and every other identifier are irreversibly shortened to a
// stable hash so a private resource name cannot leak into the local metric.
func targetMetricResourceID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	if validMetricUUID(id) {
		return "uuid:" + redactedUUIDSuffix(strings.ToLower(id))
	}
	sum := sha256.Sum256([]byte(id))
	return "sha256:" + hex.EncodeToString(sum[:8])
}

func validMetricUUID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	for i, r := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

type observedTargetProvider struct {
	primary targetProvider
	metrics targetMetricSink
}

func newObservedTargetProvider(primary targetProvider) targetProvider {
	return &observedTargetProvider{primary: primary, metrics: captureTargetMetricSink()}
}

// Resolve observes every primary outcome. During the explicit-pin compatibility
// cycle it also resolves once with only explicit target fields cleared. That
// shadow result is measured but never returned, so it cannot soften a failed
// explicit resolution or invoke an operation callback.
func (p *observedTargetProvider) Resolve(ctx context.Context, input resolveTargetInput) (resolutionResult, error) {
	if p == nil {
		result := resolutionResult{Status: resolutionUnresolved}
		writeTargetMetric(metricForTargetResult(result, nil, ""))
		return result, nil
	}
	if p.primary == nil {
		result := resolutionResult{Status: resolutionUnresolved}
		p.metrics.write(metricForTargetResult(result, nil, ""))
		return result, nil
	}
	primary, primaryErr := p.primary.Resolve(ctx, input)
	category := ""
	primaryMetric := metricForTargetResult(primary, primaryErr, category)
	if hasExplicitTarget(input) {
		category = targetMismatchLegacyUse
		primaryMetric.ResolutionRung = "explicit"
		primaryMetric.MismatchCategory = category
	}
	p.metrics.write(primaryMetric)
	if !hasExplicitTarget(input) {
		return primary, primaryErr
	}

	shadowInput := input
	shadowInput.ExplicitWorkspaceID = ""
	shadowInput.ExplicitProjectID = ""
	shadowInput.ExplicitRepositoryID = ""
	shadowInput.ExplicitRepository = repositoryIdentity{}
	launchTargetShadow(ctx, func(shadowCtx context.Context) {
		shadow, shadowErr := p.primary.Resolve(shadowCtx, shadowInput)
		p.metrics.write(metricForTargetResult(shadow, shadowErr, compareTargetResults(primary, primaryErr, shadow, shadowErr)))
	})
	return primary, primaryErr
}

func metricForTargetResult(result resolutionResult, err error, mismatch string) targetMetric {
	status := result.Status
	if err != nil {
		status = targetGateStatus(targetResolutionStatusFromError(err))
	}
	if status == "" {
		status = resolutionUnresolved
	}
	rung := "target_provider"
	if err != nil {
		rung = targetResolutionRungFromError(err)
	} else if result.Context.ResolvedBy != "" {
		rung = result.Context.ResolvedBy
	}
	return targetMetric{
		ResolutionStatus:     status,
		ResolutionRung:       rung,
		ProjectResourceID:    targetMetricResourceID(result.Context.ProjectWorkspaceID),
		RepositoryResourceID: targetMetricResourceID(result.Context.RepositoryWorkspaceID),
		MismatchCategory:     mismatch,
	}
}

func compareTargetResults(primary resolutionResult, primaryErr error, shadow resolutionResult, shadowErr error) string {
	primaryStatus := primary.Status
	if primaryErr != nil {
		primaryStatus = targetGateStatus(targetResolutionStatusFromError(primaryErr))
	}
	shadowStatus := shadow.Status
	if shadowErr != nil {
		shadowStatus = targetGateStatus(targetResolutionStatusFromError(shadowErr))
	}
	if primaryStatus != shadowStatus {
		return targetMismatchStatus
	}
	if primaryStatus != resolutionResolved {
		return targetMismatchMatch
	}
	if primary.Context.ProjectWorkspaceID != shadow.Context.ProjectWorkspaceID {
		return targetMismatchProject
	}
	if primary.Context.RepositoryWorkspaceID != shadow.Context.RepositoryWorkspaceID {
		return targetMismatchRepository
	}
	return targetMismatchMatch
}

// stateBriefSchemaMetricMiddleware observes the exact SDK output-validation
// boundary. It records only the stable failure category; the SDK error may
// contain response structure or content and is never copied into the metric.
func stateBriefSchemaMetricMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if err != nil && method == "tools/call" && strings.Contains(err.Error(), "validating tool output") {
			if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && params.Name == getStateBriefTool.Name {
				writeTargetMetric(targetMetric{
					ResolutionRung:   "state_brief_output",
					MismatchCategory: targetMismatchSchemaFailure,
				})
			}
		}
		return result, err
	}
}
