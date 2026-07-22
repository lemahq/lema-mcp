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
git_common_dir=$(git -C "$repo_root" rev-parse --path-format=absolute --git-common-dir)
public_checkout_root=$(dirname "$git_common_dir")
platform_root=${LEMA_PLATFORM_WORKTREE:-$(dirname "$public_checkout_root")/lema}
acceptance_adr_dir=${LEMA_ACCEPTANCE_ADR_DIR:-$platform_root/docs/adr}
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
  if [[ ! -d "$acceptance_adr_dir" ]]; then
    echo "candidate stdio smoke requires ADR fixtures at $acceptance_adr_dir" >&2
    return 1
  fi
  local process output log
  for process in 1 2; do
    output="$candidate_dir/mcp-$process.jsonl"
    log="$candidate_dir/mcp-$process.log"
    if ! (
      printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"target-context-acceptance","version":"1"}}}'
      sleep 1
      printf '%s\n' '{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}'
      printf '%s\n' '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
      sleep 1
      printf '%s\n' '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_state_brief","arguments":{}}}'
      sleep 1
    ) | env \
      LEMA_API_URL="http://127.0.0.1:1" \
      LEMA_API_TOKEN="acceptance-placeholder" \
      LEMA_WORKSPACE_ID="11111111-2222-3333-4444-555555555555" \
      "$candidate_dir/lema-mcp" --adr-dir "$acceptance_adr_dir" >"$output" 2>"$log"; then
      echo "candidate stdio process $process failed; inspect redacted temporary log before exit" >&2
      return 1
    fi
    if ! grep -Fq '"id":2' "$output" ||
      ! grep -Fq '"name":"get_state_brief"' "$output" ||
      ! grep -Fq '"sections":true' "$output" ||
      ! grep -Fq '"silences":true' "$output" ||
      ! grep -Fq '"id":3' "$output" ||
      ! grep -Fq '"structuredContent":{"note":"state brief unavailable: target lookup unresolved' "$output"; then
      echo "candidate stdio process $process handshake, schema, or State Brief call check failed" >&2
      return 1
    fi
    if grep -Fq 'acceptance-placeholder' "$output" ||
      grep -Fq '127.0.0.1' "$output" ||
      grep -Fq '11111111-2222-3333-4444-555555555555' "$output"; then
      echo "candidate stdio process $process leaked placeholder target inputs" >&2
      return 1
    fi
    echo "candidate: fresh stdio process $process safe State Brief unavailable call passed"
  done
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

if [[ ${1:-} == "--smoke-only" ]]; then
  exit 0
fi

if [[ $# -gt 0 ]]; then
  run_case "$1"
else
  for case_name in "${cases[@]}"; do
    run_case "$case_name"
  done
fi
