# Target context: Projects, repositories, and Runs

Hosted `lema-mcp` resolves the repository you are working in automatically. Save
an Organization-scoped credential once, leave `LEMA_WORKSPACE_ID` unset, and
restart your coding agent. Each MCP invocation then receives one immutable target
receipt; the process never keeps a mutable “active repository.”

## The model

```text
Organization
└── Project
    ├── Repository A
    ├── Repository B
    ├── Repository C
    └── Work Unit
        ├── Run: user 1, Repository A
        └── Run: user 2, Repository B
```

- **Organization** owns identity, membership, security, billing, and credentials.
  It is not a default prompt scope.
- **Project** is the explicit one-to-many repository boundary for shared state.
- **Repository** is the leaf where decisions and source material are written.
- **Work Unit** is one objective that may span repositories.
- **Run** is one user/harness execution. It belongs to the Project and retains its
  primary Repository as provenance.

Each user has their own credential. Two users can work in different repositories
of one Project and contribute Runs to the same Work Unit without sharing session
state, credentials, or an active-target variable.

## Normal setup

Obtain an API credential for the Organization you intend to use, then store it
outside every repository:

```text
# ~/.config/lema/credentials
LEMA_API_URL=https://api.lema.sh
LEMA_API_TOKEN=lema_live_...
```

```bash
chmod 600 ~/.config/lema/credentials
npx lema-mcp@latest doctor context
```

`doctor context` reports the resolution rung, canonical Repository, redacted
Organization/Project/Repository identifiers, and hashed local evidence. It never
prints the token, API URL, raw home path, or full resource IDs.

Environment values override the credentials file. A credential is scoped to one
Organization: Lema does not search other Organizations for a matching repository.
To switch Organizations, start the harness with that Organization's
`LEMA_API_URL` and `LEMA_API_TOKEN`, then restart it.

## Resolution precedence

Lema evaluates target evidence in this order and stops on stale or forbidden
explicit evidence instead of silently widening scope:

1. validated explicit compatibility override;
2. validated resumed Run;
3. host-qualified canonical Git remote for the current checkout;
4. validated `.lema/context.json` local association;
5. unresolved.

The receipt includes one Project, one primary Repository, and only the linked
repositories visible to the current credential. Read operations use that visible
set; writes stay on the primary Repository leaf; Runs and Work Units use the
Project container.

An unlinked Repository behaves as a singleton Project. Linking it later does not
change its repository identity.

## Multi-repo and multi-user behavior

Suppose a Project contains web, API, and infrastructure repositories:

- User 1 opens the web repository in Claude Code. Git resolves Repository A and
  the Project; the Run records Repository A.
- User 2 opens the API repository in Cursor with their own credential. It resolves
  Repository B and the same Project; the Run records Repository B.
- Both Runs can reference one Project-homed Work Unit.
- A State Brief leads with the primary Run, labels cross-repository evidence, and
  includes only repository leaves the requesting user can see.

Project membership never grants access to a linked Repository. Hidden leaves are
omitted without exposing their names, slugs, or IDs.

Parallel repositories, worktrees, and monorepo subdirectories are safe because
target state is immutable per invocation. Cache entries are partitioned by API
URL, credential fingerprint, canonical Repository, and explicit override.

## No remote, non-Git, or ambiguous checkout

Create a validated repository-local association:

```bash
npx lema-mcp@latest context link \
  --project <project-id> \
  --repository <repository-id>
npx lema-mcp@latest doctor context
```

The resulting `.lema/context.json` contains resource identifiers, canonical
repository identity when available, and a hash of the local root. It contains no
token, API URL, username, or raw absolute path. Lema revalidates it on every new
process.

If the checkout moves, a link changes, or access is revoked, resolution fails
closed as stale or forbidden. Preserve a recoverable backup and relink:

```bash
npx lema-mcp@latest context unlink
npx lema-mcp@latest context link \
  --project <project-id> \
  --repository <repository-id>
```

`context unlink` renames the association to `.lema/context.json.bak`; it does
not silently delete it.

## Explicit compatibility override

`LEMA_WORKSPACE_ID` remains supported for CI, recovery, and ambiguity. It may be
a visible workspace UUID or compatible configured value, but Lema validates it
against the current credential. A stale, foreign, or hidden value fails without
falling back to Git.

Do not copy `LEMA_WORKSPACE_ID` into every Claude, Codex, Cursor, repository, and
global configuration. That recreates the synchronization problem automatic
resolution removes.

## Transport boundary

This release supports local MCP over stdio. `lema-mcp serve --http` is the
localhost Workbench GUI API, not MCP Streamable HTTP. Generic remote MCP and Devin
support require the separately approved remote transport phase and are not
available through this setup.

## Troubleshooting

- **unresolved:** verify credentials, the Git `origin`, and Repository membership;
  then run `doctor context`.
- **ambiguous:** link the checkout explicitly or use a validated
  `LEMA_WORKSPACE_ID` recovery override.
- **stale:** remove or update the explicit override; for a local association, run
  `context unlink` and relink.
- **forbidden:** switch to a credential authorized for the Repository. Lema will
  not broaden to the Organization.
- **configuration changed but behavior did not:** fully restart the coding agent;
  MCP server processes retain the configuration they started with.
