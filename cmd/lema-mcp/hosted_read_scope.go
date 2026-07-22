package main

import (
	"context"
	"errors"
)

// errHostedReadScopeOutsideReceipt is intentionally generic. A rejected
// caller-supplied id may name a hidden repository, so neither the id nor any
// receipt details may cross the validation boundary.
var errHostedReadScopeOutsideReceipt = errors.New("requested workspace scope is outside the resolved target")

// hostedReadWorkspaceScope returns the canonical repository-leaf scope for one
// immutable receipt. The primary leaf always leads; remaining visible leaves
// keep their receipt order and are stably de-duplicated. A caller-provided list
// can only filter that canonical scope, never add to it or reorder it.
func hostedReadWorkspaceScope(receipt targetContext, requested []string) ([]string, error) {
	visible := make([]string, 0, len(receipt.VisibleRepositoryWorkspaceIDs))
	seen := make(map[string]struct{}, len(receipt.VisibleRepositoryWorkspaceIDs))
	add := func(id string) {
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		visible = append(visible, id)
	}
	add(receipt.RepositoryWorkspaceID)
	for _, id := range receipt.VisibleRepositoryWorkspaceIDs {
		add(id)
	}

	if len(requested) == 0 {
		return visible, nil
	}
	wanted := make(map[string]struct{}, len(requested))
	for _, id := range requested {
		if _, ok := seen[id]; !ok {
			return nil, errHostedReadScopeOutsideReceipt
		}
		wanted[id] = struct{}{}
	}
	narrowed := make([]string, 0, len(wanted))
	for _, id := range visible {
		if _, ok := wanted[id]; ok {
			narrowed = append(narrowed, id)
		}
	}
	return narrowed, nil
}

// withHostedReadScope is the common hosted-read boundary: resolve exactly one
// immutable receipt, validate/default the requested workspace scope within it,
// then permit operation HTTP. Every non-resolved status and every invalid
// narrowing returns before operation is called.
func withHostedReadScope[T any](
	ctx context.Context,
	runtime hostedWriteRuntime,
	requested []string,
	operation func(context.Context, []string, targetContext) (T, error),
) (T, error) {
	return withResolvedTarget(ctx, runtime.targets, runtime.targetInput, func(ctx context.Context, receipt targetContext) (T, error) {
		var zero T
		scope, err := hostedReadWorkspaceScope(receipt, requested)
		if err != nil {
			return zero, err
		}
		return operation(ctx, scope, receipt)
	})
}

func currentHostedRuntime() (hostedWriteRuntime, error) {
	if processHostedRuntime == nil || processHostedRuntime.hosted == nil {
		return hostedWriteRuntime{}, &targetResolutionError{status: resolutionUnresolved, rung: "hosted_runtime"}
	}
	return *processHostedRuntime, nil
}
