# Enforcement-lift benchmark — findings (2026-06-03)

**Question:** does an agent with lema's never-reopen enforcement re-propose a
DOCUMENTED-KILLED alternative less often than a baseline, on repos we didn't write?

**Setup.** Two real repos: Backstage (4 decisions) and vite (2 decisions), transcribed
faithfully from their real ADRs/docs into the `record_decision` capture format. Enforcement =
the real `lema-mcp guard` binary (ADR-0052) run on the agent's draft edit as a PreToolUse
payload in an isolated cwd (enforces only the seed). Agent + judge: Claude Sonnet 4.6 via API.
3 arms × 6 decisions × 8–12 trials = **168 trials, 0 errors.**

- **blind** — task only, no doc (the realistic case: agents don't pre-read all decisions)
- **docs** — the relevant decision pre-loaded in context (the repo-grounding / Copilot ideal)
- **lema** — no doc pre-loaded; the guard fires on the draft and the agent gets one revision round

**Metric.** Primary = a deterministic, comment-stripped code check (does the final code import
node-fetch / type with React.FC / use process.env / module.hot / `export default` / import
moment?). Cross-checked against an arm-blind LLM judge: **agreed on 163/168.** The 5
disagreements are all the deterministic check false-firing on the *nudged agent's own audit
text* ("verify no `module.hot` appears", "removed the `process.env` reference") — hand-verified;
the judge is correct on all 5. Reported numbers use the judge where they diverge.

## Result

| decision | type | blind | docs | lema |
|---|---|---|---|---|
| node-fetch → native fetch | **contrarian** | **58%** | 0% | **0%** |
| React.FC → plain function | aligned | 0% | 0% | 0% |
| default → named export | aligned | 0% | 0% | 0% |
| Moment.js → Luxon | aligned | 0% | 0% | 0% |
| process.env → import.meta.env | aligned | 0% | 0% | 0% |
| module.hot → import.meta.hot | aligned | 0% | 0% | 0% |

- **node-fetch (the one contrarian decision):** blind **58.3% (14/24, N=24 firm-up)** → lema **0%** (= docs, without carrying the doc).
- **5 model-aligned decisions:** blind 0%, lema 0%, **false-abstain 0%** across 48 trials.

## The finding

**Across two well-run public repos and six documented decisions, only ONE cuts against a 2026
frontier model.** The model already complies with the other five — including vite's
`import.meta.env`/`import.meta.hot`, which we expected to be contrarian and weren't.

This is structural, not bad luck: **public, well-documented decisions are disproportionately
the ones the model already absorbed in training** — that's *why* they're widespread enough to
be documented. So an enforcement benchmark on public repos **systematically understates**
enforcement's value. The decisions where enforcement changes behavior are the ones *outside*
the model's training — **proprietary, contrarian, recent, team-specific** — which by
construction aren't in any public repo.

So the result is a claim skeptics can't dismiss:
1. **Existence proof.** When a decision cuts against the model (node-fetch — native fetch in
   Node is recent, node-fetch is the ingrained default), an unenforced agent re-proposes the
   killed option **58%** of the time (14/24); lema → **0%**.
2. **No harm.** On decisions the model already follows, lema stays silent — 0% re-proposal,
   0% false-abstain. The nagging/false-positive failure everyone fears about enforcement did
   not occur.
3. **Implication.** Repo-grounding/training already covers public best-practices. The only
   place enforcement moves the needle is your team's **non-obvious** decisions — the exact
   argument for capture-forward, and the exact thing a fresh agent context (workflow-spawned
   or otherwise) cannot carry.

## Caveats (stated plainly)

1. The contrarian effect is **one decision (node-fetch), firmed up at N=24 (58.3%→0%)** — a
   solid existence proof, but a single decision-type; breadth across more contrarian decisions
   (which public repos supply rarely — see the finding) is the open work.
2. lema's parser auto-extracted **0** atoms from these repos' prose ADRs/docs; seeds were
   supplied via the real capture path (disclosed). Auto-extraction on real-world dialects is
   the B follow-up.
3. The honest ceiling: **a public-repo benchmark can't show the main value**, because public
   decisions are model-aligned. The real number lives on proprietary decisions and needs a
   design partner's private repo to measure.

## Positioning takeaways

- Lead with: *"enforcement catches the decisions where your team disagrees with the AI's
  defaults, and gets out of the way everywhere else"* (existence proof + no-harm).
- The value is proprietary/contrarian decisions — repo-grounding and bigger models don't reach
  them. This is the capture-forward moat, now with external evidence for the *shape* of the effect.
