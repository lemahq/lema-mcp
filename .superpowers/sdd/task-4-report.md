# Task 2.3 report — authoritative repository lookup adapters

## Status

Completed as `feat(context): make repository lookup authoritative`.

## RED / GREEN

RED, before implementation:

```text
$ go test ./cmd/lema-mcp -run 'Test(TargetResolver|Workspace|GitRemote|CredentialFingerprint)' -count=1
cmd/lema-mcp/credentials_test.go:123:11: undefined: credentialFingerprint
cmd/lema-mcp/workspace_resolve_test.go:200:37: entries[0].OrgID undefined
cmd/lema-mcp/workspace_resolve_test.go:203:16: undefined: targetWorkspacesFromEntries
cmd/lema-mcp/workspace_resolve_test.go:221:16: undefined: fetchWorkspaceLinks
FAIL github.com/lemahq/lema-mcp/cmd/lema-mcp [build failed]
```

The first full-suite run then exposed the deliberate UUID-contract migration:
old test transports did not expose configured UUIDs through `GET /workspaces`,
so the new fail-closed validation rejected them. Those fixtures were updated to
model the authoritative API listing rather than preserving UUID pass-through.

GREEN:

```text
go test ./cmd/lema-mcp -run 'Test(TargetResolver|Workspace|GitRemote)' -count=1
ok github.com/lemahq/lema-mcp/cmd/lema-mcp

go test ./cmd/lema-mcp -count=1
ok github.com/lemahq/lema-mcp/cmd/lema-mcp

go test -race ./cmd/lema-mcp -run 'Test(TargetResolver|Workspace|GitRemote)' -count=1
ok github.com/lemahq/lema-mcp/cmd/lema-mcp

go vet ./cmd/lema-mcp
git diff --check
```

## Changes

- `GET /workspaces` now decodes `org_id`, `repo_url`, and `is_repo`.
- Added authenticated `GET /workspaces/{id}/links`, plus adapters that map
  visible API responses into the pure `targetResolver` seams. No production
  operation constructs or routes through that resolver yet.
- Explicit UUIDs and slugs both validate against the visible listing. The
  cache is scoped by normalized API URL, SHA-256 credential fingerprint, and
  configured override; the token and digest are never logged.
- Git compatibility helpers now reuse host-qualified repository normalization,
  retaining the legacy `owner-repo` workspace slug while accepting HTTPS, SSH,
  SCP remotes, non-default ports, `.git`, credentials, queries, and fragments.
- Existing target resolver cache scope remains API URL, credential fingerprint,
  canonical repository, explicit Project, and path evidence as applicable.

## Files

- `cmd/lema-mcp/workspace_resolve.go`
- `cmd/lema-mcp/workspace_resolve_test.go`
- `cmd/lema-mcp/git_remote.go`
- `cmd/lema-mcp/git_remote_test.go`
- `cmd/lema-mcp/collector_sync.go`
- `cmd/lema-mcp/collector_sync_test.go`
- `cmd/lema-mcp/credentials.go`
- `cmd/lema-mcp/credentials_test.go`
- `cmd/lema-mcp/push_test.go`
- `cmd/lema-mcp/state_brief_tool_test.go`

## Concerns

- This task only installs and tests authoritative adapters. Wiring target
  resolution into production operations remains explicitly out of scope.
- The existing auto-resolving record path remains its separate legacy behavior;
  it now also validates the UUID used for its subsequent hosted push.
