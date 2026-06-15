# lema-mcp

Your coding agent reasons well — it just doesn't know *your* system, and it keeps
re-proposing the options your team already ruled out. `lema-mcp` is a local
[MCP](https://modelcontextprotocol.io) server that fixes the second problem the
way no read-only context tool can: it **captures the decisions you make** — the
chosen option *and* the alternatives you killed — and then **enforces them**, so a
settled decision stops getting reopened. No account, no database, no network.

Most "context" tools are read-only: a nicer way to grep your docs. `lema-mcp`
reads too, but its job is **never-reopen**:

- Your agent settles a choice → it calls **`record_decision`** with what it chose
  **and the alternatives it rejected, with why each was killed** (the part that
  never survives into the code).
- Before anyone's agent proposes a direction → **`check_decided`** returns the
  prior decision if that option is **CLOSED**.
- And on every edit, a **PreToolUse guard hook** (installed for you by `init`)
  reads the draft change and surfaces a CLOSED decision *before* the dead option
  gets re-proposed — enforced off both your captured decisions **and the repo's
  own ADRs**.

```bash
npx lema-mcp demo     # 30-second never-reopen walkthrough on a throwaway temp dir
npx lema-mcp init     # wire this repo for capture + enforcement (idempotent)
```

Then open the repo in your agent and ask *"why did we choose X?"* — or just let it
work, and watch it record decisions and get nudged off the ones you settled.

## Try it on React, Kubernetes, or Rust — no account

Before it knows *your* repo, `lema-mcp` can already answer **why a popular project
decided something** — over the recorded RFC/KEP decisions of React, Kubernetes,
and Rust, served from lema's public demo. No account, no token:

```bash
npx lema-mcp try react        # or: kubernetes · rust
```

That writes a **read-only** `lema-try` server to `.mcp.json` (it coexists with a
captured `lema` server — it doesn't touch your repo). Reload your agent's MCP
servers and ask:

> *"why did React adopt Hooks over mixins?"* — or, before you reach for a pattern,
> *"was a global event bus ever ruled out in Kubernetes?"*

You get **one synthesized, cited answer** — each `[n]` links to its GitHub source
(the RFC/PR) where available — and an honest **"no recorded ruling"** when the
record is silent, instead of a confident guess. Two tools light up (`public_ask`,
`why_not_public`); nothing local is registered. Ready for the same on *your own*
repo? `npx lema-mcp init` adds capture + enforcement.

Grounded only in recorded decisions; claims are **summarized, not verbatim**;
there are **no** relitigation/blast lenses (a cold import writes no
decision→decision edges) and no source-authored date. It's a curated three-repo
demo corpus, not analytics over your own graph.

## What never-reopen looks like

Your agent settles a choice and calls `record_decision` with the chosen option
and the rejected alternatives. Later, anyone's agent reaches for a dead option —
in a `check_decided` call, in a `search_decisions` result, or while drafting an
edit the guard inspects — and the killed option comes back **CLOSED**, with the
original reason attached:

> ⛔ **CLOSED — do not propose "SWR":** no first-class mutation / cache
> invalidation — we'd hand-roll it *(decided 2026-06-04 · "Data fetching for the
> web app" · chose TanStack Query)*

So the agent surfaces the prior decision instead of re-litigating it. Supersede a
decision and the *previously chosen* option goes CLOSED too — never-reopen,
enforced both ways. (That's the real output of `npx lema-mcp demo`, run against a
throwaway temp dir.)

The guard is **advisory and fail-open today**: in its default `context` mode it
injects that CLOSED note as a non-blocking nudge for the agent — it never emits an
`allow` (which would skip your normal Edit/Write confirmation on the very edit
it's flagging) and it never hard-blocks (`deny`) in v1. Opt into
`LEMA_GUARD_MODE=ask` and a strong match prompts *you* before the edit; `off` is a
kill switch. Any error → it emits nothing and gets out of the way.

Decisions are captured to `.lema/decisions.jsonl` — a plain append-only file you
can commit, so your whole team's agents share the same memory through git. No key,
no LLM call on our side: your agent forms the decision; `lema-mcp` stores it and
serves it back.

## Does enforcement actually change what the agent does?

We measured it on **two real public repos we didn't write** (Backstage, vite),
transcribing six of their own documented decisions into the `record_decision`
format and running the **real `lema-mcp guard` binary** on the agent's draft edits.
Agent and an arm-blind judge were Claude Sonnet 4.6; 168 trials, 0 errors; a
deterministic code check agreed with the judge on 163/168 (the 5 diffs
hand-verified in lema's favor). Three arms: **blind** (no doc), **docs** (the
decision pre-loaded in context), **lema** (guard only, no doc in context).

The honest result is an **existence proof**, not "agents are wrong 58% of the
time":

- Of the six well-documented public decisions, only **one** cut against the 2026
  frontier model (`node-fetch` → native `fetch`). On that one contrarian decision,
  a blind agent re-proposed the killed library **58.3% of the time (14/24, N=24)**;
  lema drove it to **0%** — matching the docs-preloaded arm *without carrying the
  doc in context*.
- On the **five decisions the model already gets right**, lema stayed silent: **0%
  re-proposal and 0% false-abstain** across 48 trials. The nagging / false-positive
  failure people fear about enforcement didn't happen.

The honest caveats, stated plainly: it's a **single contrarian decision-type**
(node-fetch, N=24) — a solid existence proof, not breadth. And a public-repo
benchmark *structurally understates* enforcement's value: public decisions are
disproportionately the ones the model already absorbed in training (that's why
they're widespread enough to be documented). The decisions where enforcement
actually moves the needle are **proprietary, contrarian, recent, team-specific** —
which by construction aren't in any public repo. That's the capture-forward thesis:
enforcement catches the decisions where your team disagrees with the AI's defaults,
and gets out of the way everywhere else.

Full method, the result table, and every raw trial: [`./docs/enforcement-lift`](./docs/enforcement-lift).

## Install

`npx` needs only Node — no Go toolchain, no account:

```bash
npx lema-mcp init                  # one-time: wire this repo for capture + enforcement
```

`init` is non-destructive and idempotent — existing config is merged, not
clobbered, and re-running changes nothing. It writes **three files** via five
idempotent steps:

1. **`.mcp.json`** — registers the `lema` MCP server (preserving any servers
   already there).
2. **`AGENTS.md`** — appends a short, managed capture-protocol block: *when you
   settle a non-trivial decision call `record_decision` with what you chose and
   rejected; before proposing a direction call `check_decided`; treat a CLOSED
   result as binding.* This protocol is what actually drives capture — a hook
   can't form the decision itself.
3. **`.claude/settings.json`** — installs **three hooks**: a PostToolUse commit
   reminder (fires only on `git commit`), the PostToolUse **capture-nudge**
   (`lema-mcp nudge`, prompts `record_decision` when you edit a dependency
   manifest), and the PreToolUse **never-reopen guard** (`lema-mcp guard`, the
   enforcement above).

`init` does **not** create `.lema/decisions.jsonl` — the capture store is created
lazily on the first `record_decision`.

Prefer Go, or want a pinned binary?

```bash
go install github.com/lemahq/lema-mcp/cmd/lema-mcp@latest
```

Or wire it by hand — add to your agent's MCP config (Claude Code `.mcp.json`).
Note this gets you the read + capture tools, but not the guard/nudge hooks that
`init` installs:

```json
{
  "mcpServers": {
    "lema": { "command": "npx", "args": ["-y", "lema-mcp@latest"] }
  }
}
```

With no flags, `lema-mcp` auto-discovers a decisions directory in the working
directory (`docs/adr`, `doc/adr`, `docs/adrs`, `docs/decisions`,
`docs/architecture/decisions`, `architecture/decisions`, `adr`, `.adr`) and an
`openspec/` tree if present. You can also point it explicitly:

```bash
lema-mcp --adr-dir docs/adr          # a local directory (no account, no network)
lema-mcp --repo github.com/org/name  # a public repo (GITHUB_TOKEN for private)
```

Other flags: `--ref` (branch/ref for a remote `--repo`), `--pattern` (ADR filename
regex), `--openspec-dir`, `--capture-file` (override the JSONL path), and
`--http`/`--port` (see *serve mode* below).

## The tools

Your agent calls these over MCP. In the standard server **eight are always on**;
`search_docs` / `get_doc` also register in local mode once a markdown tree is
indexed. (The `npx lema-mcp try` public-demo server runs a public-only subset —
just `public_ask` / `why_not_public`.)

**Enforce + capture (the part read-only tools don't have):**

- **`record_decision`** — capture a decision you just settled: the chosen option,
  the **rejected** alternatives (with why each was killed), optional rationale /
  refs / constraint / consequence, and `supersedes` to retire an earlier one.
  Rejected and superseded options come back CLOSED. Appends to
  `.lema/decisions.jsonl`.
- **`check_decided`** — before proposing a direction, check whether it's already
  decided and CLOSED. Matched off **both** the capture store **and the repo's own
  ADRs**, so a documented decision stops a fresh agent even if it was never
  captured live.

**Read (the entry point — query before you write code):**

- **`search_decisions`** — natural-language query → the most relevant atomic
  claims (chosen / rejected / constraint / consequence) with their source ADR,
  under a token budget. CLOSED flags surface here too.
- **`get_decision`** — one decision's full body, status, and typed edges.
- **`list_decisions`** — the decisions recorded in the repo, optionally by status.
- **`get_decision_graph`** — traverse typed edges (`supersedes`, `superseded_by`,
  `depends_on`, `related_to`) to connected decisions.
- **`search_docs`** / **`get_doc`** *(local mode, when a doc tree is indexed)* —
  sectioned, budgeted retrieval over the repo's project markdown (specs, READMEs,
  agent instructions, ADR/openspec full text) so the agent reads the sections that
  matter instead of whole files.

**Public demo (no account — `npx lema-mcp try <repo>`):**

- **`public_ask`** — ask why **React / Kubernetes / Rust** made a decision; one
  cited answer over their recorded RFC/KEP decisions, surfacing status and the
  ruled-out alternatives, with an honest abstain when the record is silent.
  Tokenless, over the public demo.
- **`why_not_public`** — before you propose a library / pattern / approach, check
  whether one of those projects already ruled it out: a cited answer, or a plain
  "no recorded decision against it" — which means *not on the record*, **not
  *approved***.

```
> search_decisions "why did we choose an MCP-first architecture?"
```

```jsonc
// illustrative shape — atom text/ids vary by repo
{
  "repo": "docs/adr",
  "claims": [
    { "id": "16-2", "type": "chosen",   "text": "One MCP server is the single surface agents call — all writes route through it.", "ref": "ADR-0016" },
    { "id": "27-4", "type": "rejected", "text": "Folding inference into lema was rejected on the serve path: the agent reasoning over the atoms is the customer's own.", "ref": "ADR-0027" }
  ],
  "tokens_used": 211,
  "usage": { "atoms_tokens": 211, "source_decisions": 2, "source_tokens": 1840, "tokens_saved": 1629, "compression_ratio": 8.7 },
  "truncated": false
}
```

A note on token cost: the read tools return tight, sourced atoms (default
~1500-token budget, tunable with `max_tokens`) instead of whole documents, and
every `search_decisions` response carries a self-reported `usage` block estimating
the tokens returned versus the source-document tokens for that call (the numbers
above are illustrative). On a large decision record that's a meaningful per-call
saving — but it's a local, self-measured estimate, not an audited benchmark, and
it's a side benefit. The reason to run lema is never-reopen, above.

## The subcommands

- **`init [dir]`** — wire a repo for capture (the three files / three hooks above);
  idempotent. The same code path backs the lema Workbench GUI's "enable capture"
  button.
- **`demo`** — a ~30-second never-reopen walkthrough using the real capture +
  enforce path against a throwaway temp dir that's deleted afterward. Nothing is
  written to your repo. This is the fastest way to see the CLOSED behavior.
- **`try <react|kubernetes|rust>`** — wire a **read-only public-demo** server
  (`lema-try` in `.mcp.json`, public mode) so your agent can ask why those
  projects decided things, no account. Distinct from `init` (which sets up
  capture for *your* repo); the two coexist. Reload your agent's MCP servers
  afterward.
- **`guard`** — the PreToolUse hook body: reads the tool-call payload on stdin,
  emits a never-reopen permission decision on stdout. Advisory, fail-open, always
  exits 0. `init` installs it; you don't call it directly.
- **`nudge`** — the PostToolUse hook body: on an Edit/Write/MultiEdit to a
  dependency manifest (`go.mod`, `package.json`, `cargo.toml`, `pyproject.toml`,
  `requirements.txt`, `gemfile`, `build.gradle`, `pom.xml`), emits a non-blocking
  reminder to `record_decision`. Fail-open. Installed by `init`.
- **`serve`** (≡ the `--http` flag) — serve the same engine over localhost HTTP
  (default `:4321`, `--port`) for the lema Workbench desktop GUI instead of stdio
  MCP.

## Configuration & privacy

- **`LEMA_GUARD_MODE`** — `context` (default, non-blocking nudge), `ask` (prompt
  the human on a strong match), or `off` (kill switch).
- **`LEMA_DISABLE_QUERY_LOGGING=1`** — drop query text from the usage log entirely.
  Otherwise queries are scrubbed for credential-shaped substrings before logging.
- **`LEMA_USAGE_LOG` / `LEMA_QUESTION_LOG` / `LEMA_GUARD_LOG`** — opt-in local log
  files (tool usage, unanswered questions, guard fires) for measuring and
  calibrating; all off unless set.

## Hosted retrieval (optional)

By default everything is local and lexical. To point `search_decisions` at hosted
hybrid retrieval over your full decision layer, set two env vars — no other change:

```bash
LEMA_API_URL=https://<your-lema-api> LEMA_API_TOKEN=<bearer> lema-mcp
```

In hosted mode `search_decisions` runs against the atom layer over `POST /retrieve`;
this is **search-only** in the MVP, so `get_decision` / `list_decisions` /
`get_decision_graph` return a search-only error and the doc tools aren't
registered. Capture and enforcement (`record_decision` / `check_decided` / the
guard) are **always local**.

## License

MIT. `lema-mcp` is the free, local wedge of [**lema**](https://lema.sh) — the
system of record for *why*. The hosted decision graph, the team why-surface, and
the manager-facing Intelligence layer are coming at [lema.sh](https://lema.sh).
