# Task 5.1 report — route broad reads and guard refresh

## Status

DONE

Hosted read operations now derive their workspace scope from one resolved target receipt. The canonical default is the receipt's primary repository workspace followed by the remaining visible repository workspaces in stable, de-duplicated order. Caller-supplied IDs may only narrow that set; an out-of-receipt ID rejects the complete request before operation HTTP.

## Behavior delivered

- `ask`, hosted `resolve` (`/ask` and `/retrieve`), `check`, hosted `check_approach`, `frontload`, and guard refresh use the canonical receipt scope.
- Ambiguous, forbidden, stale, and unresolved target receipts perform zero hosted operation requests.
- The public-commons `check_approach` path is unchanged.
- Frontload reuses one hosted runtime and keeps its knowledge-audit request pinned to the primary repository workspace.
- Guard refresh no longer widens to an organization workspace or reopens credentials. When target resolution fails, it preserves existing local guard behavior and emits only the redacted `guard_refresh_target_unresolved_total` diagnostic counter.
- A hidden linked repository workspace is absent from operation requests, tool output, errors, and usage logs.

## TDD evidence

Initial focused RED:

```text
go test ./cmd/lema-mcp -run '^TestTargetRoutingReadScopeDefaultsPrimaryFirstAndNarrowsWithinReceipt$' -count=1

build failed: undefined hostedReadWorkspaceScope, processHostedRuntime, and runtime hosted client
```

The same helper test passed after implementing the receipt-derived scope. Cross-family wire tests were then added for default scope, valid narrowing, invalid narrowing, every non-resolved receipt status, guard fallback/diagnostics, and hidden-leaf non-disclosure.

## Verification

All commands passed on the formatted final tree:

```text
go test ./cmd/lema-mcp -run 'Test(Ask|Resolve|Check|Guard|Frontload|TargetRouting)' -count=1
go test ./cmd/lema-mcp -count=1
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
gofmt -w <all touched Go files>
git diff --check
```

## Files

- Added `cmd/lema-mcp/hosted_read_scope.go` for canonical receipt scoping and runtime gating.
- Added `cmd/lema-mcp/target_read_routing_test.go` for cross-family routing and disclosure coverage.
- Updated hosted read handlers, frontload, guard refresh, runtime wiring, and hosted source request plumbing.
- Updated existing hosted-handler fixtures to install a resolved receipt runtime explicitly.

## Concerns

None. The worktree remains on `codex/scoped-context-reads`; no push, merge, or worktree cleanup was performed.
