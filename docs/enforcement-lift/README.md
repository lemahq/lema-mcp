# Enforcement-lift benchmark (2026-06-03)

Does an agent equipped with lema's never-reopen enforcement re-propose a
documented-killed alternative less often than a baseline, on repos we didn't write?

**Result.** On the one model-contrarian decision (node-fetch), an unenforced agent
re-proposes the killed library **58% of the time (N=24) → lema 0%**; on five
model-aligned decisions, **0% baseline and 0% false-abstain**. A deterministic code
check agreed with an arm-blind LLM judge on **163/168** trials. The strategic finding:
frontier models already comply with public best-practices, so enforcement's measurable
value lives in a team's *proprietary, contrarian* decisions — exactly the capture-forward
moat. Full writeup and caveats: [findings.md](./findings.md).

## Files

- `findings.md` — result table, interpretation, and honest caveats.
- `bench.py` — the harness. Scripts a fixed-model agent (Claude Sonnet) across three arms
  (**blind** / **docs-in-context** / **lema**), runs the **real `lema-mcp guard` binary**
  on each draft edit as a PreToolUse payload, and scores re-proposal with both a
  deterministic comment-stripped code check and an arm-blind LLM judge.
- `results-{backstage,vite,nodefetch}.json` — raw per-trial data (every agent output +
  classification), for full skeptic-transparency.

## Method (why the number survives scrutiny)

- **Killed facts enter via the real capture path** — faithful transcriptions of the source
  decisions (Backstage adr003/006/010/013–014, vite env + HMR guide) are written as
  `record_decision` JSONL, exactly what a user does. They are the repos' own decisions, not
  invented.
- **Enforcement is the real product binary**, run in an isolated cwd so it enforces only the
  seed. No reimplementation, no divergence.
- **Neutral task framing** — the agent is never told to use or avoid the killed option.
- **No self-grading** — the primary metric is a deterministic code check; the LLM judge only
  corroborates (and the two agree on 163/168, with the 5 misses being the deterministic
  check false-firing on the agent's own audit text — hand-verified).

## Reproduce

```sh
# 1. build the guard binary the harness shells out to
go -C apps/api build -o /tmp/lema-bench/lema-mcp ./cmd/lema-mcp
mkdir -p /tmp/lema-bench/guardrun

# 2. point the harness at a model and run
export ANTHROPIC_API_KEY=sk-...
python3 bench.py 8                 # all cases, N=8 per cell
python3 bench.py 24 node-fetch     # one case, N=24

# optional: relocate the working dir
export LEMA_BENCH_DIR=/path/to/workdir
```

The harness seeds the killed decisions to `$LEMA_BENCH_DIR/seed.jsonl`, runs the guard with
`cwd=$LEMA_BENCH_DIR/guardrun` (empty, so only the seed is enforced), and writes per-trial
results to `$LEMA_BENCH_DIR/results.json`.
