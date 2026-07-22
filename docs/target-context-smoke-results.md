# Target Context local acceptance smoke

Date: 2026-07-22

Scope: the approved local stdio slice only. Remote HTTP and Devin are Phase 8 and were not exercised.

## Release-candidate process

The candidate was built from this worktree with:

```sh
candidate_dir=$(mktemp -d /tmp/lema-mcp-rc.XXXXXX)
go build -trimpath -o "$candidate_dir/lema-mcp" ./cmd/lema-mcp
```

The acceptance runner launches the candidate as a fresh stdio MCP process twice. Each process completes `initialize`, `tools/list`, and `tools/call` for `get_state_brief`. Reproduce only this retained black-box smoke with `./scripts/target-context-acceptance.sh --smoke-only`.

| Check | Result | Evidence |
|---|---|---|
| Fresh-process MCP handshake | Pass | The runner asserts three JSON-RPC responses in each of two independent candidate processes: initialize, tools list, and tool call. |
| State Brief tool after restart | Pass | In both processes, `get_state_brief` is present; `sections` and `silences` advertise the intentionally permissive current SDK contract; the call validates and returns only the expected safe `target lookup unresolved` note. Placeholder API URL, token, and UUID are asserted absent from output. |
| Target resolution without a workspace pin | Pass (manual) | A credentialed `lema-mcp doctor context` run resolved by `canonical_git` from this checkout. The receipt identified `git:github.com/lemahq/lema-mcp`. |
| Cwd evidence privacy | Pass (manual) | Only `sha256:0eba6216afb5fa36` appeared for both cwd and Git root; no raw path appeared. |
| Receipt privacy | Pass (manual) | Diagnostics exposed only Project suffix `…db2d3af5` and Repository suffix `…db2d3af5`; no full workspace ID, credential, API URL, or cwd appeared. |
| Live non-empty State Brief | Pass | After installing published `lema-mcp@0.21.1` and restarting Codex Desktop, `get_state_brief` returned a non-empty structured response with `as_of`, `note`, `scope`, four populated `sections`, and six `silences`. The retained evidence omits Run IDs and decision payloads. This proves the published #40 schema fix and Desktop restart path; it does not prove #42/#43, which were not included in `0.21.1`. |

The already-running Codex Desktop MCP call failed before restart at SDK output validation: that process advertised integer `silences/items` while the server returned strings. This is the stale pre-update process signature. After the `0.21.1` install and restart, the same live tool returned a non-empty brief without schema failure. Because `0.21.1` was tagged before #42 and #43 merged, pin-free Target Context still requires the follow-up release from this branch.

## Installed harnesses

| Harness | Installed version | Stdio/context restart evidence | Result |
|---|---|---|---|
| Claude Code | `2.1.216` | Separate `claude mcp list` and `claude mcp get lema` invocations each health-checked the configured `npx -y lema-mcp@latest` server as connected. The installed configuration targets the published package, not the temporary candidate. | Current published server connected; candidate-specific client restart not performed because that would mutate shared Project configuration. |
| Codex CLI/Desktop | `codex-cli 0.145.0-alpha.27` bundled at `/Applications/ChatGPT.app/Contents/Resources/codex` | `codex mcp list` does not contain a configured Lema server. The Desktop task has an injected Lema server; after installing `0.21.1` and restarting, its live State Brief call returned a non-empty valid response. | Pass for restart and the published #40 schema fix. Pin-free #42/#43 behavior remains pending its follow-up release. |
| Cursor desktop CLI | `3.12.30` (`arm64`, build `63a2996a10d9e476b6c28e951dd7691d9c0cf480`) | The desktop launcher has configuration controls but no headless tool-call surface. | Version/config surface verified. |
| Cursor Agent | `2026.07.20-8cc9c0b` | Two fresh commands, `cursor agent mcp list` and `cursor agent mcp list-tools lema`, reported Lema ready and exposed 11 tools including `get_state_brief`. | Current published server ready; candidate-specific client restart not performed because it would mutate shared Cursor configuration. |

Running `cursor agent --help` caused Cursor's launcher to install the previously missing `cursor-agent` at `~/.local/bin/cursor-agent`; the table records the installed version. No credentials or raw workspace IDs were printed.

## Automated local matrix

Run the complete named corpus from the public repository:

```sh
./scripts/target-context-acceptance.sh
```

The runner auto-discovers a sibling `lema` platform checkout from Git's common
directory, including when the public repository is itself a worktree. If the
platform checkout lives elsewhere, set
`LEMA_PLATFORM_WORKTREE=/path/to/lema ./scripts/target-context-acceptance.sh`.

The runner builds a candidate in a private `mktemp` directory, performs two fresh stdio initialize/tools-list/State-Brief-call cycles, verifies the current schema and safe unavailable response, and removes the candidate on exit. It fails if any named local case does not execute its expected test. The `two-users` case additionally requires the platform test `TestProjectWorkUnitRetainsTwoUserRunAttribution`; absence of that test is a failure, not a skip. Database-backed cases use `LEMA_TEST_DATABASE_URL` and fail if the platform checkout or Postgres prerequisite is unavailable.

The local cases are: one repository; parallel repositories; two users/credential partitions; cross-repository Project/Work Unit; ambiguous Project; stale override; worktree/nested cwd; fork versus upstream; authoritative repository rename; Organization transfer; monorepo; no remote/non-Git; enterprise host; hidden leaf; and legacy UUID pin.

On 2026-07-22 the complete runner passed all 15 named cases, including the DB-backed platform contracts. The live non-empty State Brief and Codex Desktop restart gates also passed after installing `0.21.1`; a later release is still required to distribute #42/#43 themselves.

Remote HTTP and Devin are deliberately absent from `--list`. They are not silently skipped local cases; they belong to the separately approved Phase 8 acceptance boundary.
