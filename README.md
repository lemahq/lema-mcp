# lema-mcp

Your coding agent reasons well — it just doesn't know *your* system. `lema-mcp`
makes a repository's decisions — the chosen approach, the constraints, the
consequences, and the alternatives that were ruled out — queryable over
[MCP](https://modelcontextprotocol.io). No account, no database, no network, no
setup.

It parses a repo's `docs/adr/` (or any ADR-style decision records) and serves
four tools over stdio. Point your agent at it and it learns the constraints and
what was already tried *before* it writes code.

## ~71× fewer tokens

Instead of dumping whole ADRs into context, `search_decisions` returns tight,
sourced claims. Measured on lema's own decision record (39 ADRs, 390 claims, a
20-query goldset): each query returns roughly **220 tokens of atoms** versus
**~15,600 tokens** of the ADRs those atoms cite — a **~71× average compression**
(range 12–162×). Every `search_decisions` call carries its own `usage` block, so
the number is per-call and self-reported, not a marketing figure.

> The honest version: the baseline is the *full bodies of the ADRs the answer
> cites* — "what you'd read to reconstruct this" — and it assumes the agent reads
> each cited ADR in full. It's a favorable framing because lema's own ADRs are
> large, and on smaller decisions the ratio shrinks. The durable claim isn't the
> exact multiple; it's the order of magnitude: hundreds of focused atom-tokens
> where the source is tens of thousands.

## Example

```
> search_decisions "why did we choose an MCP-first architecture?"
```

```json
{
  "repo": "github.com/lemahq/lema",
  "claims": [
    { "id": "16-2", "type": "chosen", "text": "One MCP server is the single surface agents call — all writes route through it — with a mixed model strategy (Claude via Anthropic, Gemini + embeddings via Vertex) underneath.", "ref": "ADR-0016" },
    { "id": "27-1", "type": "constraint", "text": "The free wedge is npx lema-mcp on the user's machine — no account, no GCP — and the tool contract is identical to hosted, so graduating swaps an endpoint, not a workflow.", "ref": "ADR-0027" },
    { "id": "27-4", "type": "rejected", "text": "Folding inference into lema was rejected on the serve path: the agent reasoning over the atoms is the customer's own (Claude Code, Cursor), so lema returns atoms, not answers.", "ref": "ADR-0027" }
  ],
  "tokens_used": 211,
  "usage": {
    "atoms_tokens": 211,
    "source_decisions": 7,
    "source_tokens": 31851,
    "tokens_saved": 31640,
    "compression_ratio": 151
  },
  "truncated": false
}
```

`tokens_used` is the cost of what you got back; `usage.source_tokens` is what you
*didn't* have to read. `compression_ratio` and `tokens_saved` are the difference.

## Install

```bash
go install github.com/lemahq/lema-mcp/cmd/lema-mcp@latest   # or grab a release binary
```

Add it to your agent's MCP config (Claude Code `.mcp.json` shown):

```json
{
  "mcpServers": {
    "lema": {
      "command": "lema-mcp",
      "args": ["--repo", "github.com/yourorg/yourrepo"]
    }
  }
}
```

## Run

```bash
lema-mcp --adr-dir docs/adr              # a local directory (no account, no network)
lema-mcp --repo github.com/org/name      # a public repo (GITHUB_TOKEN for private)
```

With no flags, `lema-mcp` auto-discovers `docs/adr`, `doc/adr`, `docs/decisions`
(and an `openspec/` tree) in the working directory. Then ask your agent "why did
we choose X?" — it gets tight, sourced claims back, not whole documents.

### Hosted retrieval (optional)

By default everything is local and lexical. To point `search_decisions` at
hosted hybrid retrieval over your full decision layer ([ADR-0040](https://github.com/lemahq/lema)),
set two env vars — no other change:

```bash
LEMA_API_URL=https://<your-lema-api> LEMA_API_TOKEN=<bearer> lema-mcp
```

In hosted mode `search_decisions` runs against the atom layer over `POST /retrieve`;
`get_decision` / `list_decisions` / `get_decision_graph` stay local in this MVP.

## Tools

- **`search_decisions`** — natural-language query → the most relevant atomic
  claims (chosen / rejected / constraint / consequence) with their source ADR,
  within a token budget, plus the `usage` (tokens-saved) block.
- **`get_decision`** — one decision's full body, status, and typed edges by ADR
  number; drill down after a search surfaces a relevant `ref`.
- **`list_decisions`** — the decisions recorded in the repo, optionally filtered
  by status.
- **`get_decision_graph`** — traverse typed edges (`supersedes`,
  `superseded_by`, `depends_on`, `related_to`) from a decision to its connected
  decisions.

## License

MIT. The open-source wedge of [lema](https://github.com/lemahq/lema) — the
system of record for *why*.
