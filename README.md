# lema-mcp

A local, database-less [MCP](https://modelcontextprotocol.io) server that makes
a repository's decisions — the chosen approach, the constraints, the
consequences, and the alternatives that were ruled out — queryable by any coding
agent. No account, no network, no setup.

> "We are drowning in information, while starving for wisdom." — E. O. Wilson

It parses a repo's `docs/adr/` (or any ADR-style decision records) and serves
four tools over stdio: `search_decisions`, `get_decision`, `list_decisions`,
`get_decision_graph`. Point your agent at it and it learns the constraints and
what was already tried *before* it writes code.

## Run

```bash
go run ./cmd/lema-mcp --adr-dir docs/adr        # a local directory
go run ./cmd/lema-mcp --repo github.com/org/name # a public repo (GITHUB_TOKEN for private)
```

Add it to your agent's MCP config and ask "why did we choose X?" — you get tight,
sourced claims, not whole documents.

## License

MIT. This is the open-source wedge of [lema](https://github.com/lemahq/lema) —
the system of record for *why*.
