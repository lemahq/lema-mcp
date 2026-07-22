# Task 4.3 report — route collector Runs to Project context

## RED

`TestCollectorSyncHomesCrossRepositoryRunsOnOneProject` was added before the
production change and run with:

```text
go test ./cmd/lema-mcp -run '^TestCollectorSyncHomesCrossRepositoryRunsOnOneProject$' -count=1
```

It failed as intended: both Run creates used the frontend repository leaf
`22222222-2222-2222-2222-222222222222`, while the test required the shared
Project container `11111111-1111-1111-1111-111111111111`.

## GREEN

Collector boundary sync now constructs one `hostedWriteRuntime` using the
checkpoint cwd, resolves exactly one immutable receipt through
`withResolvedTarget`, and uses that receipt's `ProjectWorkspaceID` for both
the Run create and checkpoint-event routes. It leaves repository, branch, and
worktree evidence in the Run-create body. The syncer does not mutate an active
target.

Route and body evidence:

- frontend and API receipts both post `/workspaces/{ProjectWorkspaceID}/runs`
  and their events beneath that same container;
- their bodies retain `repo: acme/frontend` and `repo: acme/api` respectively,
  so the API can associate both Runs with one Project-homed Work Unit;
- a singleton receipt with equal Project and Repository IDs posts both calls to
  that repository leaf UUID;
- unresolved, ambiguous, forbidden, stale, and malformed receipts perform one
  resolution attempt and zero operation HTTP requests.

The former collector-specific remote-to-slug target path was removed. The
compatibility test now uses a real temporary Git checkout so it exercises the
shared resolver's canonical Git evidence rather than a retired adapter stub.

## Files

- `cmd/lema-mcp/collector_sync.go`
- `cmd/lema-mcp/collector_sync_test.go`
- `.superpowers/sdd/task-4.3-report.md`

## Commit

`feat(runs): home collector runs on project context`

## Verification

```text
go test ./cmd/lema-mcp -run '^TestCollectorSyncHomesCrossRepositoryRunsOnOneProject$' -count=1  # RED then GREEN
go test ./cmd/lema-mcp -run '^TestSyncResolvesTargetFromCanonicalGitRemote$' -count=1
go test ./cmd/lema-mcp -run '^TestCollector' -count=1
go test ./cmd/lema-mcp -count=1
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
git diff --check
```

## Concerns

None. `get_state_brief` retains its legacy `collectorSyncer` transport helper;
it does not enter the collector boundary sync path and is intentionally left to
the Phase 5 read-routing work.
