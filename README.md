# lema-mcp

**Your coding agent can read the code. It can't read the _argument_ behind it.**
`lema-mcp` gives your agent the recorded *why* — and the alternatives a project
already ruled out — cited to the source. For React, Kubernetes, and Rust out of
the box, and for your own repo with one command.

[![npm](https://img.shields.io/npm/v/lema-mcp)](https://www.npmjs.com/package/lema-mcp)
[![license](https://img.shields.io/npm/l/lema-mcp)](./LICENSE)
[![MCP](https://img.shields.io/badge/MCP-server-111)](https://modelcontextprotocol.io)

A local [MCP](https://modelcontextprotocol.io) server. No account, no database, no
network for your own repo — install it in 30 seconds and ask why a project decided
something, or whether the approach you're about to take was already rejected.

---

## ❌ Without lema

- The agent invents the **why** from training recall — fluently, and sometimes
  wrongly. The rationale lives in RFC / KEP / PR threads; it was never in the code.
- It re-proposes the approach the maintainers **already rejected** two years ago —
  because a rejected alternative leaves *no trace* in the source.
- Ask "was this ever ruled out?" and you get a confident guess, with no way to tell
  a real ruling from a hallucinated one.

## ✅ With lema

- One **cited** answer from the project's actual recorded deliberation — every `[n]`
  links to the RFC / PR where the call was made.
- A typed **`ruled_out`** verdict when a project already rejected your approach,
  with the recorded reason *and* a pointer to where the docs say to do it instead.
- A typed **`settled`** verdict when your approach *is* the project's in-force
  recorded choice — the governing decision cited, with a docs pointer for the how.
- An honest **"no recorded ruling"** when the record is silent — which means
  *unknown*, **not** *approved*. lema never fills the gap with a guess.

> lema holds **reasoning** — why a decision was made, what was rejected — not API
> syntax or code samples. For those, reach for a docs tool. lema is the right place
> for *why*.

---

## Try it in 30 seconds — no account

```bash
npx lema-mcp try react        # or: kubernetes · rust
```

That writes a read-only public server to your project's `.mcp.json`. Reload your
agent's MCP servers (in Claude Code: `/mcp`) and try the flagship tool,
**`check_approach`** — name a direction, get the recorded verdict:

```text
> "Let's add a delayMs prop to Suspense to debounce the fallback."   (repo: react)

  ⛔ ruled_out — the React team considered and rejected this.
     "<the recorded rationale, summarized — not a quote>"  [1]
     Where to look instead →  https://react.dev/reference/react
     [1] reactjs/rfcs#212

> "I'll add a global event bus for cross-component communication."   (repo: react)

  ◦ no_recorded_ruling — React's public record doesn't settle this.
     (Unknown — not approved.)
```

Or just ask in plain language — *"why did React adopt Hooks over mixins?"* — and
get one cited answer, with an honest abstain when the record is silent.

Covered today: **React · Kubernetes · Rust**, served from lema's public API
(`api.lema.sh`). Tokenless. It's a curated three-project demo corpus — not
analytics over a graph you own.

---

## Install

**One click — installs the no-account public demo** (React's recorded decisions, zero setup):

[![Add lema to Cursor](https://cursor.com/deeplink/mcp-install-dark.svg)](https://cursor.com/en/install-mcp?name=lema&config=eyJjb21tYW5kIjoibnB4IiwiYXJncyI6WyIteSIsImxlbWEtbWNwQGxhdGVzdCJdLCJlbnYiOnsiTEVNQV9NQ1BfTU9ERSI6InB1YmxpYyIsIkxFTUFfUFVCTElDX1JFUE8iOiJyZWFjdC1yZmNzIn19) &nbsp; [![Install lema in VS Code](https://img.shields.io/badge/VS_Code-Install_lema-0098FF?style=flat-square&logo=visualstudiocode&logoColor=white)](https://vscode.dev/redirect?url=vscode%3Amcp%2Finstall%3F%257B%2522name%2522%253A%2522lema%2522%252C%2522command%2522%253A%2522npx%2522%252C%2522args%2522%253A%255B%2522-y%2522%252C%2522lema-mcp%2540latest%2522%255D%252C%2522env%2522%253A%257B%2522LEMA_MCP_MODE%2522%253A%2522public%2522%252C%2522LEMA_PUBLIC_REPO%2522%253A%2522react-rfcs%2522%257D%257D)

The button drops you straight into React's public record — ask *"why did React rule out X?"* and get a cited answer, no account. To wire **your own repo** for capture, or point the demo at Kubernetes or Rust, use the per-client setup below.

`npx` needs only Node — no Go toolchain, no account. Two commands cover both ways
to use lema:

```bash
npx lema-mcp try react   # read-only: ask React/Kubernetes/Rust why + what's ruled out
npx lema-mcp init        # your repo: decision capture + the never-reopen guard
```

Both are **non-destructive and idempotent** — they merge into existing config and
re-running changes nothing. `init` and `try` share the same `lema` server key; the
authed `init` server is a superset (it serves the public tools too), so the two
coexist and `try` never downgrades it.

<details>
<summary><b>Claude Code</b></summary>

Easiest — let lema write the config and hooks for you:

```bash
npx lema-mcp init        # or: npx lema-mcp try react
```

Or add it by hand to `.mcp.json` (this gets the read + capture tools, but not the
guard/nudge hooks that `init` installs):

```json
{
  "mcpServers": {
    "lema": { "command": "npx", "args": ["-y", "lema-mcp@latest"] }
  }
}
```
</details>

<details>
<summary><b>Cursor</b></summary>

Add to `.cursor/mcp.json` (project) or `~/.cursor/mcp.json` (global):

```json
{
  "mcpServers": {
    "lema": { "command": "npx", "args": ["-y", "lema-mcp@latest"] }
  }
}
```

For the no-account public demo instead, add the env block:

```json
{
  "mcpServers": {
    "lema": {
      "command": "npx",
      "args": ["-y", "lema-mcp@latest"],
      "env": { "LEMA_MCP_MODE": "public", "LEMA_PUBLIC_REPO": "react-rfcs" }
    }
  }
}
```
</details>

<details>
<summary><b>Claude Desktop</b></summary>

Settings → Developer → Edit Config, then add to `mcpServers`:

```json
{
  "mcpServers": {
    "lema": {
      "command": "npx",
      "args": ["-y", "lema-mcp@latest"],
      "env": { "LEMA_MCP_MODE": "public", "LEMA_PUBLIC_REPO": "react-rfcs" }
    }
  }
}
```
</details>

<details>
<summary><b>Windsurf</b></summary>

Add to `~/.codeium/windsurf/mcp_config.json`:

```json
{
  "mcpServers": {
    "lema": { "command": "npx", "args": ["-y", "lema-mcp@latest"] }
  }
}
```
</details>

<details>
<summary><b>VS Code (GitHub Copilot)</b></summary>

Add to `.vscode/mcp.json` — note VS Code uses the `servers` key:

```json
{
  "servers": {
    "lema": { "command": "npx", "args": ["-y", "lema-mcp@latest"] }
  }
}
```
</details>

<details>
<summary><b>Go install / pinned binary</b></summary>

```bash
go install github.com/lemahq/lema-mcp/cmd/lema-mcp@latest
```

For the public demo, set `LEMA_MCP_MODE=public` and `LEMA_PUBLIC_REPO=react-rfcs`
(`k8s-enhancements` · `rust-rfcs`). The public API URL is baked into the binary.
</details>

The public config sets only `LEMA_MCP_MODE` + `LEMA_PUBLIC_REPO`
(`react-rfcs` · `k8s-enhancements` · `rust-rfcs`) — the API URL is compiled in.

---

## Two ways to use it

### 1. The public record — React, Kubernetes, Rust (read-only, no account)

Ask why a popular project decided something, or check whether a direction was
already rejected, over its recorded RFC/KEP deliberation. This is the `try` server.

### 2. Your own repo — capture + never-reopen (local, no account)

Most "context" tools are read-only — a nicer way to grep your docs. lema reads too,
but its real job is **never-reopen**:

- Your agent settles a choice → it calls **`record_decision`** with the option it
  chose **and the alternatives it rejected, with why each was killed** (the part
  that never survives into the code).
- Before anyone proposes a direction → **`check_decided`** returns the prior
  decision if that option is **CLOSED**.
- On every edit, a **PreToolUse guard hook** (installed by `init`) reads the draft
  change and surfaces a CLOSED decision *before* the dead option gets re-proposed —
  enforced off both your captured decisions **and the repo's own ADRs**.

Decisions are captured to `.lema/decisions.jsonl` — a plain append-only file you
commit, so your whole team's agents share the same memory through git. No key, no
LLM call on our side: your agent forms the decision; lema stores it and serves it
back.

#### What never-reopen looks like

Your agent reaches for an option you already killed — and it comes back **CLOSED**,
with the original reason attached:

> ⛔ **CLOSED — do not propose "SWR":** no first-class mutation / cache
> invalidation — we'd hand-roll it *(decided 2026-06-04 · "Data fetching for the
> web app" · chose TanStack Query)*

So the agent surfaces the prior decision instead of re-litigating it. Supersede a
decision and the *previously chosen* option goes CLOSED too — enforced both ways.
(That's the real output of `npx lema-mcp demo`, run against a throwaway temp dir.)

The guard is **advisory and fail-open**: in its default `context` mode it injects
that note as a non-blocking nudge — it never hard-blocks and never auto-approves an
edit. `LEMA_GUARD_MODE=ask` prompts *you* on a strong match; `off` is a kill switch.
Any error → it emits nothing and gets out of the way.

---

## Available tools

Your agent calls these over MCP.

### The public record (no account)

| Tool | What it does |
|------|--------------|
| **`check_approach`** ★ | Name an approach → a three-valued verdict: `ruled_out` (rejected, with the recorded **why** synthesized and cited), `settled` (it *is* the project's in-force recorded choice, the governing decision cited), or an honest `no_recorded_ruling`. Every verdict carries a pointer to where the docs cover the how. The one public door — it folds in the cited "why was this decided?" answer (the former `why_decided`) and the `settled` check. |

### Your own repo

| Tool | What it does |
|------|--------------|
| **`record_decision`** | Capture a settled decision: the chosen option and the **rejected** alternatives (with why each was killed), plus rationale / refs / `supersedes`. Rejected and superseded options come back CLOSED. Append-only. |
| **`check_decided`** | Adjudicate one proposed direction against decisions already CLOSED → typed verdict (`ruled_out` / `not_ruled_out` / `incomplete` / `error`), off **both** your capture store **and** the repo's ADRs. |
| **`search_decisions`** | Natural-language query → the most relevant atomic claims (chosen / rejected / constraint / consequence) with their source ADR, under a token budget. |
| **`get_decision`** · **`list_decisions`** · **`get_decision_graph`** | One decision's full body; the list by status; traversal of typed edges (`supersedes`, `depends_on`, …). |
| **`search_docs`** · **`get_doc`** | Sectioned, budgeted retrieval over the repo's project markdown (local mode, once a doc tree is indexed) — the matching sections, not whole files. |
| **`ask`** | One cited, synthesized answer over your team's **hosted** decision graph (hosted mode). |

In your own repo the full server registers the read + capture tools (and the public
`check_approach` too); the `npx lema-mcp try` server runs the public door only.

### `lema settle` — rule from the terminal (hosted mode)

The package also installs a `lema` command. `settle` is the terminal half of
adjudication: it **drafts** a ruling on a hosted decision and prints the deep
link where your **browser click binds it** — a terminal credential never
binds anything (that split is structural: programmatic principals cannot
create binding rulings, by design).

```
lema settle accept <decision-id>...              # draft an accept, print the bind link
lema settle reject <decision-id> --reason <why>  # proposed drafts only
lema settle supersede <decision-id> --by <decision-id>
```

Decision ids can be full UUIDs or unique 6+ character prefixes. Requires
hosted credentials (`LEMA_API_URL`, `LEMA_API_TOKEN`, `LEMA_WORKSPACE_ID`).

---

## Why lema is different (the honest part)

lema's brand *is* its honesty — that's what makes a "why" tool trustworthy:

- **Abstain ≠ approval.** Silence is reported as silence. lema would rather say
  "no recorded ruling" than manufacture one.
- **Cited, summarized — not quoted.** Answers are grounded in recorded decisions and
  paraphrased ("the record indicates …"), each claim tied to a followable ref.
- **Local-first.** Capture and enforcement run entirely on your machine, in a file
  you own. No key, no upload, no model call on our side.
- **No fabricated graph.** A cold import writes no decision→decision edges and no
  source-authored dates; lema shows what's actually on the record, nothing it can't
  stand behind.

---

## Does enforcement change what the agent does?

We measured it on **two real public repos we didn't write** (Backstage, vite),
transcribing six of their documented decisions into `record_decision` format and
running the **real `lema-mcp guard` binary** on the agent's draft edits. 168 trials,
0 errors. The honest result is an **existence proof**, not "agents are wrong 58% of
the time":

- On the one decision that cut against the 2026 frontier model (`node-fetch` →
  native `fetch`), a blind agent re-proposed the killed library **58.3%** of the
  time (14/24); lema drove it to **0%** — matching a docs-preloaded arm *without*
  carrying the doc in context.
- On the five decisions the model already gets right, lema stayed silent: **0%
  re-proposal and 0% false-abstain** across 48 trials. No nagging.

A public-repo benchmark *understates* the value — public decisions are
disproportionately the ones the model already absorbed in training. The decisions
where enforcement moves the needle are proprietary, contrarian, recent,
team-specific. Full method and every raw trial:
[`./docs/enforcement-lift`](./docs/enforcement-lift).

---

## Configuration & privacy

- **`LEMA_GUARD_MODE`** — `context` (default, non-blocking), `ask` (prompt the human
  on a strong match), or `off`.
- **`LEMA_DISABLE_QUERY_LOGGING=1`** — drop query text from the usage log entirely.
  Otherwise queries are scrubbed for credential-shaped substrings before logging.
- **`LEMA_USAGE_LOG` / `LEMA_QUESTION_LOG` / `LEMA_GUARD_LOG`** — opt-in local log
  files; all off unless set.

<details>
<summary><b>Subcommands & flags</b></summary>

- **`init [dir]`** — wire a repo for capture: registers the server in `.mcp.json`,
  appends a managed capture-protocol block to `AGENTS.md`, and installs three hooks
  (a commit reminder, the `nudge` capture prompt on dependency-manifest edits, and
  the `guard` never-reopen check). Idempotent.
- **`try <react|kubernetes|rust>`** — wire the read-only public-demo server.
- **`demo`** — a ~30-second never-reopen walkthrough against a throwaway temp dir
  (nothing written to your repo). The fastest way to see the CLOSED behavior.
- **`guard`** / **`nudge`** — the hook bodies `init` installs; advisory, fail-open,
  always exit 0. You don't call them directly.
- **`serve`** (≡ `--http`, default `:4321`) — serve the same engine over localhost
  HTTP for the lema Workbench desktop GUI instead of stdio MCP.

With no flags, lema auto-discovers a decisions directory (`docs/adr`, `doc/adr`,
`docs/adrs`, `docs/decisions`, `docs/architecture/decisions`,
`architecture/decisions`, `adr`, `.adr`) and an `openspec/` tree. Point it
explicitly with `--adr-dir`, `--repo github.com/org/name` (`GITHUB_TOKEN` for
private), `--ref`, `--pattern`, `--openspec-dir`, or `--capture-file`.

**Hosted retrieval (optional).** Set `LEMA_API_URL` + `LEMA_API_TOKEN` to point
`search_decisions` at hosted hybrid retrieval over your full decision layer
(search-only in the MVP). Capture and enforcement are always local.
</details>

---

## License

MIT. `lema-mcp` is the free, local wedge of [**lema**](https://lema.sh) — the system
of record for *why*. The hosted decision graph, the team why-surface, and the
manager-facing Intelligence layer are at [lema.sh](https://lema.sh).
