# lema-mcp

Your coding agent reasons well — it just doesn't know *your* system, and it keeps
re-proposing things your team already ruled out. `lema-mcp` fixes both, locally,
over [MCP](https://modelcontextprotocol.io). No account, no database, no network.

It gives your agent three things:

1. **The *why* behind past decisions** — what you chose, the constraints, the
   consequences, and crucially the alternatives you *ruled out* — queryable
   just-in-time, as tight sourced claims instead of whole documents.
2. **A way to record new decisions as it works** — so the next session (yours or a
   teammate's) inherits the reasoning instead of re-deriving it.
3. **Enforcement of the ones you killed** — ask about a rejected or superseded
   option and it comes back **CLOSED**, so your agent stops re-suggesting dead ends.

```bash
npx lema-mcp init     # writes .mcp.json + an AGENTS.md capture protocol + a commit reminder
```

Then open the repo in your agent and ask *"why did we choose X?"* — or just let it
work, and watch it record decisions and refuse to reopen settled ones.

## What makes it different: it remembers what you *ruled out*

Most "context" tools are read-only — a nicer way to grep docs. `lema-mcp` also
**captures** decisions as your agent makes them and **enforces** them:

- Your agent settles a choice → it calls **`record_decision`** with what it chose
  **and the alternatives it rejected** (the part that never makes it into the code).
- Later, anyone's agent considers a dead option → **`check_decided`** returns it
  **CLOSED**:

  > ⛔ **CLOSED — do not propose "SWR":** no first-class mutation/cache invalidation —
  > we'd hand-roll it. *(decided 2026-05-31 · "State management for the web app" · chose TanStack Query)*

  …so it surfaces the prior decision instead of re-litigating it. Supersede a
  decision and the *previously chosen* option goes CLOSED too — never-reopen,
  enforced, not just remembered.

Decisions are captured to `.lema/decisions.jsonl` — a plain append-only file you
can commit, so your whole team's agents share the same memory through git. No
key, no LLM call on our side: your agent forms the decision; `lema-mcp` stores it
and serves it back.

## ~71× fewer tokens

The read path is built to be cheap. Instead of dumping whole ADRs into context,
`search_decisions` returns tight, sourced claims. Measured on lema's own decision
record (39 ADRs, 390 claims, a 20-query goldset): each query returns roughly
**220 tokens of atoms** versus **~15,600 tokens** of the ADRs those atoms cite —
a **~71× average compression** (range 12–162×). Every `search_decisions` call
carries its own `usage` block, so the number is per-call and self-reported, not a
marketing figure.

> The honest version: the baseline is the *full bodies of the ADRs the answer
> cites* — "what you'd read to reconstruct this" — and it assumes the agent reads
> each cited ADR in full. It's a favorable framing because lema's own ADRs are
> large, and on smaller decisions the ratio shrinks. The durable claim isn't the
> exact multiple; it's the order of magnitude: hundreds of focused atom-tokens
> where the source is tens of thousands.

```
> search_decisions "why did we choose an MCP-first architecture?"
```

```json
{
  "repo": "github.com/lemahq/lema",
  "claims": [
    { "id": "16-2", "type": "chosen", "text": "One MCP server is the single surface agents call — all writes route through it — with a mixed model strategy (Claude via Anthropic, Gemini + embeddings via Vertex) underneath.", "ref": "ADR-0016" },
    { "id": "27-4", "type": "rejected", "text": "Folding inference into lema was rejected on the serve path: the agent reasoning over the atoms is the customer's own (Claude Code, Cursor), so lema returns atoms, not answers.", "ref": "ADR-0027" }
  ],
  "tokens_used": 211,
  "usage": { "atoms_tokens": 211, "source_decisions": 7, "source_tokens": 31851, "tokens_saved": 31640, "compression_ratio": 151 },
  "truncated": false
}
```

## Install

`npx` needs only Node — no Go toolchain, no account:

```bash
npx lema-mcp init                  # one-time: wires this repo for capture + enforcement
```

`init` is non-destructive and idempotent: it registers the server in `.mcp.json`,
appends a short capture protocol to `AGENTS.md`/`CLAUDE.md`, and adds a
commit-time reminder hook. Prefer Go, or want a pinned binary?

```bash
go install github.com/lemahq/lema-mcp/cmd/lema-mcp@latest
```

Or wire it by hand — add to your agent's MCP config (Claude Code `.mcp.json`):

```json
{
  "mcpServers": {
    "lema": { "command": "npx", "args": ["-y", "lema-mcp@latest"] }
  }
}
```

With no flags, `lema-mcp` auto-discovers `docs/adr`, `doc/adr`, `docs/decisions`
(and an `openspec/` tree) in the working directory. You can also point it at a
directory or a public repo:

```bash
lema-mcp --adr-dir docs/adr          # a local directory (no account, no network)
lema-mcp --repo github.com/org/name  # a public repo (GITHUB_TOKEN for private)
```

## The tools

Read:

- **`search_decisions`** — natural-language query → the most relevant atomic
  claims (chosen / rejected / constraint / consequence) with their source, within
  a token budget, plus the `usage` (tokens-saved) block. Captured decisions and
  **CLOSED** flags surface here too.
- **`get_decision`** — one decision's full body, status, and typed edges.
- **`list_decisions`** — the decisions recorded in the repo, optionally by status.
- **`get_decision_graph`** — traverse typed edges (`supersedes`, `superseded_by`,
  `depends_on`, `related_to`) to connected decisions.

Write + enforce:

- **`record_decision`** — capture a decision: the chosen option, the **rejected**
  alternatives (with why), optional rationale / refs / constraint / consequence,
  and `supersedes` to retire an earlier one. Appends to `.lema/decisions.jsonl`.
- **`check_decided`** — before proposing a direction, ask whether it's already
  settled. Returns any **CLOSED** decisions that rule the option out.

## Hosted retrieval (optional)

By default everything is local and lexical. To point `search_decisions` at hosted
hybrid retrieval over your full decision layer, set two env vars — no other change:

```bash
LEMA_API_URL=https://<your-lema-api> LEMA_API_TOKEN=<bearer> lema-mcp
```

In hosted mode `search_decisions` runs against the atom layer over `POST /retrieve`;
the other tools stay local in this MVP. Capture (`record_decision` /
`check_decided`) is always local.

## License

MIT. The open-source wedge of [lema](https://github.com/lemahq/lema) — the system
of record for *why*.
