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
| Live non-empty State Brief | **Blocked** | Separate credentialed candidate calls from both the public and platform checkout returned a structured, redacted receipt note but reported `no prior run known`; they therefore had no `sections`/`silences` response to retain. A known hosted Run or a local checkpoint for the selected checkout is the missing prerequisite. This gate remains failed; the output was not fabricated. |

The already-running Codex Desktop MCP call failed before restart at SDK output validation: that process advertised integer `silences/items` while the server returned strings. This is the stale pre-update process signature. The fresh candidate no longer advertised that schema, but the live non-empty-brief gate still requires a known Run.

## Installed harnesses

| Harness | Installed version | Stdio/context restart evidence | Result |
|---|---|---|---|
| Claude Code | `2.1.216` | Separate `claude mcp list` and `claude mcp get lema` invocations each health-checked the configured `npx -y lema-mcp@latest` server as connected. The installed configuration targets the published package, not the temporary candidate. | Current published server connected; candidate-specific client restart not performed because that would mutate shared Project configuration. |
| Codex CLI/Desktop | `codex-cli 0.145.0-alpha.27` bundled at `/Applications/ChatGPT.app/Contents/Resources/codex` | `codex mcp list` does not contain a configured Lema server. The current Desktop task has an injected Lema server, but its pre-restart State Brief call showed the stale schema failure above. | **Blocked:** restart the Desktop task against a merged/published candidate, then rerun the live State Brief call. |
| Cursor desktop CLI | `3.12.30` (`arm64`, build `63a2996a10d9e476b6c28e951dd7691d9c0cf480`) | The desktop launcher has configuration controls but no headless tool-call surface. | Version/config surface verified. |
| Cursor Agent | `2026.07.20-8cc9c0b` | Two fresh commands, `cursor agent mcp list` and `cursor agent mcp list-tools lema`, reported Lema ready and exposed 11 tools including `get_state_brief`. | Current published server ready; candidate-specific client restart not performed because it would mutate shared Cursor configuration. |

Running `cursor agent --help` caused Cursor's launcher to install the previously missing `cursor-agent` at `~/.local/bin/cursor-agent`; the table records the installed version. No credentials or raw workspace IDs were printed.

## Automated local matrix

Run the complete named corpus from the public repository:

```sh
LEMA_PLATFORM_WORKTREE=/Users/andrew/Projects/lema/worktrees/project-brief \
  ./scripts/target-context-acceptance.sh
```

The runner builds a candidate in a private `mktemp` directory, performs two fresh stdio initialize/tools-list/State-Brief-call cycles, verifies the current schema and safe unavailable response, and removes the candidate on exit. It fails if any named local case does not execute its expected test. The `two-users` case additionally requires the platform test `TestProjectWorkUnitRetainsTwoUserRunAttribution`; absence of that test is a failure, not a skip. Database-backed cases use `LEMA_TEST_DATABASE_URL` and fail if the platform checkout or Postgres prerequisite is unavailable.

The local cases are: one repository; parallel repositories; two users/credential partitions; cross-repository Project/Work Unit; ambiguous Project; stale override; worktree/nested cwd; fork versus upstream; authoritative repository rename; Organization transfer; monorepo; no remote/non-Git; enterprise host; hidden leaf; and legacy UUID pin.

On 2026-07-22 the complete runner passed all 15 named cases, including the DB-backed platform contracts. The separately listed live non-empty State Brief and Codex Desktop restart gates remain blocked.

Remote HTTP and Devin are deliberately absent from `--list`. They are not silently skipped local cases; they belong to the separately approved Phase 8 acceptance boundary.
