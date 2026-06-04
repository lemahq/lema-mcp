#!/usr/bin/env python3
"""
lema enforcement-lift benchmark (A: supplied-atoms, real guard).

Measures whether an agent re-proposes a DOCUMENTED-KILLED alternative from a real
external repo (Backstage), across three arms:
  - blind: no ADR in context (the realistic case — agents don't pre-read all ADRs)
  - docs:  the relevant ADR in context (the GitHub Copilot Memory / repo-grounding ideal)
  - lema:  no ADR in context, but the REAL `lema-mcp guard` (ADR-0052) runs on the
           agent's draft and, if it reaches for a killed option, injects the product
           nudge; the agent gets one revision round.

Killed facts are seeded via the real capture JSONL path (record_decision format),
faithfully transcribed from Backstage's accepted ADRs (adr003/006/014). The guard
is the real built binary, run in an empty cwd so it enforces ONLY the seed.

Judge: a separate, arm-blind Sonnet call classifies the final answer
(uses_killed? completed?). Neutral task framing — the agent is never told to use
or avoid the killed option.
"""
import concurrent.futures as cf
import json
import os
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

API_URL = "https://api.anthropic.com/v1/messages"
MODEL = "claude-sonnet-4-6"
API_KEY = os.environ.get("ANTHROPIC_API_KEY", "")

BENCH = os.environ.get("LEMA_BENCH_DIR", "/tmp/lema-bench")
MCP_BIN = f"{BENCH}/lema-mcp"
SEED_PATH = f"{BENCH}/seed.jsonl"
GUARD_DIR = f"{BENCH}/guardrun"   # empty dir => guard enforces only the seed, no stray ADRs
RESULTS = f"{BENCH}/results.json"

ARMS = ["blind", "docs", "lema"]
N = int(sys.argv[1]) if len(sys.argv) > 1 else 8
MAX_WORKERS = 2  # low-tier key: high concurrency trips HTTP 429

AGENT_SYSTEM = (
    "You are a senior engineer contributing to the Backstage open-source project. "
    "Write idiomatic, modern TypeScript that follows Backstage's conventions. "
    "Provide the code with a brief explanation."
)

# Faithful transcriptions of the real Backstage ADRs (Context + Decision).
ADR006 = (
    "ADR006: Avoid React.FC and React.SFC. Facebook removed React.FC from their base "
    "TypeScript template because it added next to no benefit with downsides: children "
    "props were implicitly added, and the generic type was not supported on children. "
    "Decision: React.FC and React.SFC should be avoided when adding new code. Instead "
    "write a plain function component and type its props explicitly, e.g. "
    "`const GoodComponent = ({ text, children }: GoodProps) => ...`, adding "
    "`children?: React.ReactNode` only when needed."
)
ADR014 = (
    "ADR014: Proper use of HTTP fetching libraries. Backstage previously recommended "
    "node-fetch in Node.js contexts (ADR013). The minimum requirement has since been "
    "upgraded to Node.js 20+, which ships a stable native fetch (undici-based), and "
    "third-party libraries caused issues on newer Node. Decision: all code executed in "
    "Node.js (backend and CLIs) should use the native `fetch` for HTTP data fetching, "
    "and `typeof fetch` as the TypeScript type where an implementation is injected. "
    "Transition away from third-party fetch replacement packages such as node-fetch."
)
ADR003 = (
    "ADR003: Avoid Default Exports and Prefer Named Exports. Default exports add "
    "indirection, increase cognitive load, thwart IDE rename/refactor tools, promote "
    "typos, are awkward in CommonJS interop, and break re-exports due to name "
    "conflicts. Named exports make 'Find All References', 'Go To Definition', and grep "
    "reliable. Decision: stop using default exports except when absolutely necessary "
    "(such as React.lazy modules); prefer named exports, e.g. "
    "`export const ConnectedComponent = ...`."
)
ADR010 = (
    "ADR010: Use the Luxon Date Library. Date formatting (e.g. 'a day ago') and "
    "calculations are common in Backstage. Moment.js has commonly filled this gap but "
    "suffers from large bundle sizes and mutable state, and is being sunset; the "
    "project recommends modern alternatives. Decision: use Luxon as the standard date "
    "library within Backstage. Luxon offers a similar feature set and API to Moment.js "
    "but with immutability and modern JavaScript APIs (e.g. Intl), yielding smaller "
    "bundles. All core packages and plugins should use Luxon for date manipulation or "
    "formatting that the native Date object cannot easily handle."
)
VITE_ENV = (
    "Vite — Env Variables and Modes. Vite exposes env variables under the special "
    "`import.meta.env` object, statically replaced at build time. Variables must be "
    "prefixed with `VITE_` to be exposed to client-side source code, e.g. "
    "`import.meta.env.VITE_API_URL`. Vite does NOT expose Node's `process.env` to "
    "client source code — using `process.env` in client code does not work. Use "
    "`import.meta.env` for environment access in Vite apps."
)
VITE_HMR = (
    "Vite — HMR API. Vite exposes its manual Hot Module Replacement API via the "
    "special `import.meta.hot` object, e.g. `if (import.meta.hot) { "
    "import.meta.hot.accept(cb) }` and `import.meta.hot.dispose(cb)`. This replaces "
    "webpack's `module.hot`; Vite code should guard with `import.meta.hot` and call "
    "`import.meta.hot.accept`, not `module.hot`."
)

CASES = [
    {
        "id": "react-fc",
        "task": ("Create a new React component for Backstage called UserCard that "
                 "displays a user's name and role. It should accept `name` and `role` "
                 "string props. Write it in TypeScript."),
        "killed": "React.FC (or React.SFC) to type the component",
        "chosen": "a plain function component with an explicit props type",
        "seed_title": "React component typing convention",
        "seed_chosen": "plain function with typed props object",
        "seed_option": "React.FC",
        "seed_why": ("implicit children prop and no generic children support; removed "
                     "from the standard TS template (ADR006)"),
        "adr_ref": "ADR006",
        "adr_text": ADR006,
    },
    {
        "id": "node-fetch",
        "task": ("Write a TypeScript function for a Backstage backend plugin that "
                 "fetches a JSON list of users from https://example.com/api/users and "
                 "returns it. Include the necessary import."),
        "killed": "the node-fetch package",
        "chosen": "the native global fetch",
        "seed_title": "HTTP fetching in Node backend",
        "seed_chosen": "native global fetch",
        "seed_option": "node-fetch",
        "seed_why": ("Backstage requires Node 20+ with stable native fetch; third-party "
                     "libs caused issues (ADR014 supersedes ADR013)"),
        "adr_ref": "ADR014",
        "adr_text": ADR014,
    },
    {
        "id": "default-export",
        "task": ("Add a small TypeScript utility module for Backstage called "
                 "formatTimestamp that formats a Date as a relative time string (e.g. "
                 "'3 days ago'), and show how it is exported for use by other modules."),
        "killed": "a default export",
        "chosen": "a named export",
        "seed_title": "Module export style",
        "seed_chosen": "named export",
        "seed_option": "default export",
        "seed_why": "adds indirection, thwarts refactoring tools, promotes typos (ADR003)",
        "adr_ref": "ADR003",
        "adr_text": ADR003,
    },
    {
        "id": "moment-luxon",
        "task": ("In a Backstage plugin, write a TypeScript function that takes an "
                 "ISO 8601 timestamp string and returns a human-readable relative time "
                 "like '3 days ago'. Include the import for whatever date library you use."),
        "killed": "the Moment.js library (the `moment` package)",
        "chosen": "Luxon (or another non-Moment approach)",
        "seed_title": "Standard date library",
        "seed_chosen": "Luxon",
        "seed_option": "moment",
        "seed_why": ("large bundle size, mutable state, and being sunset; Luxon is the "
                     "standard (ADR010)"),
        "adr_ref": "ADR010",
        "adr_text": ADR010,
    },
    {
        "id": "vite-env",
        "task": ("In a Vite + TypeScript client app, write a small module that reads the "
                 "API base URL from an environment variable and exports a configured "
                 "fetch helper. Show how you read the environment variable."),
        "killed": "Node's process.env to read the environment variable",
        "chosen": "import.meta.env (Vite's env mechanism)",
        "seed_title": "Environment variable access in client code",
        "seed_chosen": "import.meta.env.VITE_*",
        "seed_option": "process.env",
        "seed_why": ("Vite does not expose process.env to client source code; use "
                     "import.meta.env.VITE_* (Vite env docs)"),
        "adr_ref": "Vite env docs",
        "adr_text": VITE_ENV,
    },
    {
        "id": "vite-hmr",
        "task": ("In a Vite app, write a module that uses Hot Module Replacement so that "
                 "when this module changes it accepts the update and re-runs its setup. "
                 "Show the HMR code."),
        "killed": "webpack's module.hot HMR API",
        "chosen": "import.meta.hot (Vite's HMR API)",
        "seed_title": "Hot Module Replacement API",
        "seed_chosen": "import.meta.hot",
        "seed_option": "module.hot",
        "seed_why": ("Vite exposes HMR via import.meta.hot, not webpack's module.hot "
                     "(Vite HMR API docs)"),
        "adr_ref": "Vite HMR docs",
        "adr_text": VITE_HMR,
    },
]
# Optional case filter: argv[2] = comma-separated case ids (run a subset).
_flt = sys.argv[2].split(",") if len(sys.argv) > 2 else None
if _flt:
    CASES = [c for c in CASES if c["id"] in _flt]

_print_lock = threading.Lock()
_tok_lock = threading.Lock()
_tokens = {"in": 0, "out": 0}


def log(msg):
    with _print_lock:
        print(msg, flush=True)


def write_seed():
    with open(SEED_PATH, "w") as f:
        for c in CASES:
            rec = {
                "id": "d_seed_" + c["id"].replace("-", ""),
                "ts": "2026-06-03T00:00Z",
                "title": c["seed_title"],
                "chosen": c["seed_chosen"],
                "rejected": [{"option": c["seed_option"], "why": c["seed_why"]}],
                "status": "accepted",
            }
            f.write(json.dumps(rec) + "\n")
    os.makedirs(GUARD_DIR, exist_ok=True)


def call_claude(system, messages, temp, max_tokens):
    body = json.dumps({
        "model": MODEL, "max_tokens": max_tokens, "system": system,
        "temperature": temp, "messages": messages,
    }).encode()
    last = ""
    for attempt in range(8):
        req = urllib.request.Request(API_URL, data=body, method="POST", headers={
            "x-api-key": API_KEY, "anthropic-version": "2023-06-01",
            "content-type": "application/json",
        })
        try:
            with urllib.request.urlopen(req, timeout=180) as r:
                data = json.loads(r.read())
        except urllib.error.HTTPError as e:
            code = e.code
            last = f"HTTP {code}"
            if code == 429 or code >= 500:
                ra = e.headers.get("retry-after") if e.headers else None
                try:
                    wait = float(ra)
                except (TypeError, ValueError):
                    wait = min(2 ** attempt, 30)
                time.sleep(wait + 0.5)
                continue
            raise RuntimeError(f"{last}: {e.read()[:200]!r}")
        except Exception as ex:  # network blip
            last = str(ex)
            time.sleep(min(2 ** attempt, 20))
            continue
        text = "".join(p.get("text", "") for p in data.get("content", []))
        u = data.get("usage", {})
        with _tok_lock:
            _tokens["in"] += u.get("input_tokens", 0)
            _tokens["out"] += u.get("output_tokens", 0)
        return text, u.get("input_tokens", 0), u.get("output_tokens", 0)
    raise RuntimeError(f"api failed after retries ({last})")


def run_guard(draft):
    """Run the REAL lema-mcp guard on a draft edit; return (nudge, fired)."""
    payload = json.dumps({
        "tool_name": "Write",
        "tool_input": {"file_path": "Component.tsx", "content": draft},
    })
    p = subprocess.run([MCP_BIN, "guard", "--capture-file", SEED_PATH],
                       input=payload, capture_output=True, text=True, cwd=GUARD_DIR)
    out = (p.stdout or "").strip()
    if not out:
        return "", False
    try:
        o = json.loads(out)["hookSpecificOutput"]
    except Exception:
        return "", False
    ctx = o.get("additionalContext") or o.get("permissionDecisionReason") or ""
    return ctx, bool(ctx)


def agent_final(case, arm):
    """Produce the agent's final answer for one (case, arm). Returns dict."""
    task = case["task"]
    if arm == "docs":
        user = ("Relevant conventions from the repository's architecture decision "
                f"records:\n\n{case['adr_text']}\n\n---\n\nTask: {task}")
    else:
        user = task
    msgs = [{"role": "user", "content": user}]
    draft, _, _ = call_claude(AGENT_SYSTEM, msgs, 1.0, 1200)
    nudge_fired = False
    final = draft
    if arm == "lema":
        nudge, fired = run_guard(draft)
        nudge_fired = fired
        if fired:
            msgs.append({"role": "assistant", "content": draft})
            msgs.append({"role": "user", "content": (
                "An automated pre-commit check (lema never-reopen) flagged your code:\n\n"
                f"{nudge}\n\nRevise your code to respect this settled team decision "
                "while still completing the task.")})
            final, _, _ = call_claude(AGENT_SYSTEM, msgs, 1.0, 1200)
    return {"final": final, "nudge_fired": nudge_fired}


JUDGE_SYSTEM = "You are a strict, impartial code reviewer. Respond with ONLY a JSON object."


def judge(case, code):
    prompt = (
        f"A developer was asked to do this task:\n\nTASK: {case['task']}\n\n"
        f"They produced this answer:\n---\n{code}\n---\n\n"
        f"Q1 (uses_killed): Does the answer ACTUALLY USE {case['killed']}? "
        f"Answer true only if the final code employs it. If it instead uses "
        f"{case['chosen']}, or only mentions the alternative in order to avoid it, "
        f"answer false.\n"
        f"Q2 (completed): Does the answer accomplish the task with a working solution? "
        f"Answer false only if it refuses, is empty, or fails to address the task.\n\n"
        'Respond with exactly: {"uses_killed": <true|false>, "completed": <true|false>, '
        '"evidence": "<short quote or reason>"}'
    )
    out, _, _ = call_claude(JUDGE_SYSTEM, [{"role": "user", "content": prompt}], 0.0, 400)
    s, e = out.find("{"), out.rfind("}")
    try:
        r = json.loads(out[s:e + 1])
        return bool(r.get("uses_killed")), bool(r.get("completed")), r.get("evidence", "")
    except Exception:
        return None, None, f"UNPARSEABLE: {out[:120]}"


def trial(case, arm, i):
    try:
        res = agent_final(case, arm)
        uses, done, ev = judge(case, res["final"])
        rec = {"case": case["id"], "arm": arm, "i": i, "uses_killed": uses,
               "completed": done, "nudge_fired": res["nudge_fired"], "evidence": ev,
               "final": res["final"]}
        mark = "?" if uses is None else ("RE-PROPOSED" if uses else "clean")
        log(f"  [{arm:5}] {case['id']:14} #{i}  {mark}"
            + ("  (nudged)" if res["nudge_fired"] else ""))
        return rec
    except Exception as ex:
        log(f"  [{arm:5}] {case['id']:14} #{i}  ERROR {ex}")
        return {"case": case["id"], "arm": arm, "i": i, "error": str(ex)}


def main():
    if not API_KEY:
        sys.exit("ANTHROPIC_API_KEY not set (source /tmp/lema-bench/.env)")
    write_seed()
    log(f"enforcement-lift benchmark · model={MODEL} · N={N}/cell · "
        f"{len(CASES)} cases × {len(ARMS)} arms = {len(CASES)*len(ARMS)*N} trials\n")
    jobs = [(c, arm, i) for c in CASES for arm in ARMS for i in range(N)]
    results = []
    with cf.ThreadPoolExecutor(max_workers=MAX_WORKERS) as ex:
        for rec in ex.map(lambda a: trial(*a), jobs):
            results.append(rec)

    with open(RESULTS, "w") as f:
        json.dump(results, f, indent=2)

    def agg(rows):
        ok = [r for r in rows if "error" not in r and r.get("uses_killed") is not None]
        n = len(ok)
        if n == 0:
            return None
        rep = sum(1 for r in ok if r["uses_killed"]) / n
        fab = sum(1 for r in ok if not r["completed"]) / n
        nud = sum(1 for r in ok if r.get("nudge_fired")) / n
        return {"n": n, "reproposal": rep, "false_abstain": fab, "nudge_fire": nud}

    log("\n" + "=" * 64)
    log("ENFORCEMENT-LIFT RESULTS  (re-proposal rate of the killed option)")
    log("=" * 64)
    log(f"{'arm':6} {'n':>3}  {'re-propose':>11}  {'false-abstain':>13}  {'nudge-fire':>10}")
    by_arm = {}
    for arm in ARMS:
        a = agg([r for r in results if r["arm"] == arm])
        by_arm[arm] = a
        if a:
            log(f"{arm:6} {a['n']:>3}  {a['reproposal']*100:>9.1f}%  "
                f"{a['false_abstain']*100:>11.1f}%  {a['nudge_fire']*100:>8.1f}%")

    log("\nper-case re-proposal rate:")
    log(f"{'case':16} " + "  ".join(f"{arm:>8}" for arm in ARMS))
    for c in CASES:
        cells = []
        for arm in ARMS:
            a = agg([r for r in results if r["arm"] == arm and r["case"] == c["id"]])
            cells.append(f"{a['reproposal']*100:>7.0f}%" if a else "    n/a")
        log(f"{c['id']:16} " + "  ".join(cells))

    b, d, l = by_arm.get("blind"), by_arm.get("docs"), by_arm.get("lema")
    if b and l:
        log("\nlift:")
        log(f"  lema vs blind:  {b['reproposal']*100:.1f}% -> {l['reproposal']*100:.1f}% "
            f"re-proposal  ({(b['reproposal']-l['reproposal'])*100:+.1f} pts)")
    if d and l:
        log(f"  lema vs docs:   {d['reproposal']*100:.1f}% -> {l['reproposal']*100:.1f}% "
            f"re-proposal  ({(d['reproposal']-l['reproposal'])*100:+.1f} pts)")
    log(f"\ntokens used: {_tokens['in']:,} in / {_tokens['out']:,} out")
    log(f"per-trial detail: {RESULTS}")


if __name__ == "__main__":
    main()
