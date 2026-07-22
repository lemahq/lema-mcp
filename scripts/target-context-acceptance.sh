#!/usr/bin/env bash
set -euo pipefail

cases=(
  one-repo
  parallel-repos
  two-users
  cross-repo-work-unit
  ambiguous-project
  stale-override
  worktree
  fork-upstream-distinct
  repository-rename
  organization-transfer
  monorepo
  no-remote
  enterprise-host
  hidden-leaf
  legacy-uuid
)

if [[ ${1:-} == "--list" ]]; then
  printf '%s\n' "${cases[@]}"
  exit 0
fi

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
platform_root=${LEMA_PLATFORM_WORKTREE:-/Users/andrew/Projects/lema/worktrees/project-brief}
candidate_dir=$(mktemp -d /tmp/lema-mcp-target-context.XXXXXX)
trap 'rm -rf -- "$candidate_dir"' EXIT

run_public() {
  local case_name=$1
  local pattern=$2
  local expected=$3
  local output
  echo "acceptance: $case_name"
  if ! output=$(cd "$repo_root" && go test ./cmd/lema-mcp -run "$pattern" -count=1 -v 2>&1); then
    printf '%s\n' "$output"
    return 1
  fi
  printf '%s\n' "$output"
  if ! grep -Fq -- "--- PASS: $expected" <<<"$output"; then
    echo "acceptance case $case_name did not execute expected test $expected" >&2
    return 1
  fi
}

run_platform() {
  local case_name=$1
  local package=$2
  local pattern=$3
  local expected=$4
  local output
  if [[ ! -d "$platform_root/apps/api" ]]; then
    echo "acceptance case $case_name requires LEMA_PLATFORM_WORKTREE; none found at $platform_root" >&2
    return 1
  fi
  echo "acceptance: $case_name (platform contract)"
  if ! output=$(cd "$platform_root/apps/api" && \
    LEMA_TEST_DATABASE_URL="${LEMA_TEST_DATABASE_URL:-postgres://lema:lema-dev@localhost:5432/lema_test?sslmode=disable}" \
    go test "$package" -run "$pattern" -count=1 -v 2>&1); then
    printf '%s\n' "$output"
    return 1
  fi
  printf '%s\n' "$output"
  if ! grep -Fq -- "--- PASS: $expected" <<<"$output"; then
    echo "acceptance case $case_name did not execute expected test $expected" >&2
    return 1
  fi
}

smoke_candidate() {
  local adr_dir="$platform_root/docs/adr"
  local output="$candidate_dir/mcp.jsonl"
  local log="$candidate_dir/mcp.log"
  local smoke_home="$candidate_dir/home"
  if [[ ! -d "$adr_dir" ]]; then
    echo "candidate stdio smoke requires platform ADR fixtures at $adr_dir" >&2
    return 1
  fi
  mkdir -p "$smoke_home"
  if ! (
    printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"target-context-acceptance","version":"1"}}}'
    sleep 1
    printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}'
    printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
    sleep 1
  ) | env \
    HOME="$smoke_home" \
    LEMA_API_URL="http://127.0.0.1:1" \
    LEMA_API_TOKEN="acceptance-placeholder" \
    LEMA_WORKSPACE_ID= \
    "$candidate_dir/lema-mcp" --adr-dir "$adr_dir" >"$output" 2>"$log"; then
    echo "candidate stdio process failed; inspect redacted temporary log before exit" >&2
    return 1
  fi
  if ! grep -Fq '"id":2' "$output" ||
    ! grep -Fq '"name":"get_state_brief"' "$output" ||
    ! grep -Fq '"sections":true' "$output" ||
    ! grep -Fq '"silences":true' "$output"; then
    echo "candidate stdio handshake or State Brief schema check failed" >&2
    return 1
  fi
  echo "candidate: fresh stdio handshake and State Brief schema passed"
}

run_case() {
  case "$1" in
    one-repo)
      run_public "$1" '^TestStateBriefWithoutPinResolvesVerifiedGitAndRoutesProject$' 'TestStateBriefWithoutPinResolvesVerifiedGitAndRoutesProject'
      ;;
    parallel-repos)
      run_public "$1" '^TestTargetResolverParallelRepositoryReceiptsRemainIsolated$' 'TestTargetResolverParallelRepositoryReceiptsRemainIsolated'
      ;;
    two-users)
      run_public "$1" '^TestTargetResolverCredentialPartitionsRemainIsolated$' 'TestTargetResolverCredentialPartitionsRemainIsolated'
      run_platform "$1" './internal/api' '^TestProjectWorkUnitRetainsTwoUserRunAttribution$' 'TestProjectWorkUnitRetainsTwoUserRunAttribution'
      ;;
    cross-repo-work-unit)
      run_public "$1" '^TestCollectorSyncHomesCrossRepositoryRunsOnOneProject$' 'TestCollectorSyncHomesCrossRepositoryRunsOnOneProject'
      run_platform "$1" './internal/api' '^TestStateBriefProjectVisibleRepositories$' 'TestStateBriefProjectVisibleRepositories'
      ;;
    ambiguous-project)
      run_public "$1" '^TestTargetResolverProjectParents$' 'TestTargetResolverProjectParents'
      ;;
    stale-override)
      run_public "$1" '^TestStateBriefStaleExplicitOverrideSendsNoOperationAndDoesNotFallbackToGit$' 'TestStateBriefStaleExplicitOverrideSendsNoOperationAndDoesNotFallbackToGit'
      ;;
    worktree)
      run_public "$1" '^TestTargetResolverWorktreesAndNestedCWDKeepRepositoryButHashPaths$' 'TestTargetResolverWorktreesAndNestedCWDKeepRepositoryButHashPaths'
      ;;
    fork-upstream-distinct)
      run_public "$1" '^TestTargetResolverForkOriginRemainsDistinctFromUpstream$' 'TestTargetResolverForkOriginRemainsDistinctFromUpstream'
      ;;
    repository-rename)
      run_public "$1" '^TestTargetResolverAuthoritativeRenameRetainsRepositoryWorkspaceIdentity$' 'TestTargetResolverAuthoritativeRenameRetainsRepositoryWorkspaceIdentity'
      ;;
    organization-transfer)
      run_public "$1" '^TestTargetResolverOrganizationTransferRejectsStaleReceipt$' 'TestTargetResolverOrganizationTransferRejectsStaleReceipt'
      ;;
    monorepo)
      run_public "$1" '^TestTargetResolverMonorepoSubdirectoriesShareOneRepositoryReceipt$' 'TestTargetResolverMonorepoSubdirectoriesShareOneRepositoryReceipt'
      ;;
    no-remote)
      run_public "$1" '^TestContextLinkSupportsNoRemoteAndNonGitAtRoot$' 'TestContextLinkSupportsNoRemoteAndNonGitAtRoot'
      ;;
    enterprise-host)
      run_public "$1" '^TestTargetResolverRepositoryIdentityIncludesHost$' 'TestTargetResolverRepositoryIdentityIncludesHost'
      ;;
    hidden-leaf)
      run_public "$1" '^TestTargetRoutingHiddenLeafAbsentFromRequestOutputErrorAndUsageLog$' 'TestTargetRoutingHiddenLeafAbsentFromRequestOutputErrorAndUsageLog'
      run_platform "$1" './internal/api' '^TestStateBriefProjectVisibleRepositories$' 'TestStateBriefProjectVisibleRepositories'
      ;;
    legacy-uuid)
      run_public "$1" '^TestStateBriefLegacyUUIDPinRoutesAuthoritativelyAndRedactsSuffix$' 'TestStateBriefLegacyUUIDPinRoutesAuthoritativelyAndRedactsSuffix'
      ;;
    *)
      echo "unknown local acceptance case: $1" >&2
      return 2
      ;;
  esac
}

cd "$repo_root"
go build -trimpath -o "$candidate_dir/lema-mcp" ./cmd/lema-mcp
echo "candidate: built in redacted temporary directory"
smoke_candidate

if [[ $# -gt 0 ]]; then
  run_case "$1"
else
  for case_name in "${cases[@]}"; do
    run_case "$case_name"
  done
fi
