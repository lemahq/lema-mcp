package main

import "github.com/lemahq/lema-mcp/internal/source"

// Shared state + honesty-boundary strings for the public (tokenless) surface. They
// were extracted from public_ask_tool.go when the why_decided tool was dropped
// (ADR-0124, the "one door" fold): check_approach — the surviving public door —
// depends on all of them (runCheckApproach), so they outlive the why_decided handler.
// Each CTA/note rides in an OUTPUT field as DATA, never in a selection-time tool
// description (no steering, per the Directory criteria), and costs tokens only on the
// path that sets it.

// publicSrc is the tokenless public client; nil when LEMA_PUBLIC_API_URL is
// unset and no default is baked in (check_approach then fails loud at call time).
var publicSrc *source.Public

// publicRepoSlugs maps the user-facing repo arg (and common aliases) to the
// seeded public workspace slug. Keep in sync with lema-demo-seed/repos.go.
var publicRepoSlugs = map[string]string{
	"react":            "react-rfcs",
	"react-rfcs":       "react-rfcs",
	"kubernetes":       "k8s-enhancements",
	"k8s":              "k8s-enhancements",
	"k8s-enhancements": "k8s-enhancements",
	"rust":             "rust-rfcs",
	"rust-rfcs":        "rust-rfcs",
}

// abstainUpgradeCTA is the honest conversion line on an abstain: the public graph
// genuinely cannot answer about the user's private repo, so connecting it adds a
// DIFFERENT corpus (more value) — it is not ransom on the answer just declined.
const abstainUpgradeCTA = "No recorded ruling matched in the public graph — and it doesn't include your own repo's decisions. To get cited why-answers grounded in YOUR team's record, connect your repo: https://lema.sh/?utm_source=lema-mcp&utm_medium=public_ask&utm_campaign=abstain"

// rateLimitedUpgradeCTA is the honest convert on a 429 (free public quota hit):
// the cap is reached, the answer is not withheld — point to the account/own-repo
// path for more, never "pay to unlock this answer".
const rateLimitedUpgradeCTA = "For higher limits — and cited why-answers grounded in YOUR own repo — create an account and connect it: https://lema.sh/?utm_source=lema-mcp&utm_medium=public_ask&utm_campaign=rate_limited"

// groundingNote rides with every GROUNDED answer: it tells the consuming agent to
// relay the [n]-cited claims as the project's recorded decisions and keep its own
// model recall clearly separate — closing the synthesis-time blur where an agent
// folds general knowledge in among the real citations under a "from the record"
// banner. Costs output tokens only on grounded calls, never on an abstain.
const groundingNote = "The [n]-cited claims are this project's recorded decisions — relay them as the record. Keep any of your own general knowledge separate and labeled; don't fold it into the citations."

// publicGroundedCaveats are the honest limits of a grounded public answer: the
// capabilities lema has on a connected repo but NOT on a cold public import.
// They ride in the `caveats` output field (not the selection-time description)
// so the consuming agent can surface them as data without us steering it, and
// so they cost tokens only when there is a grounded claim to qualify. Each line
// names one thing the public surface does NOT do — guarding the overclaim trap
// where a cited answer reads as if the full decision graph were behind it.
var publicGroundedCaveats = []string{
	"No decision-to-decision graph here: superseding or related rulings in the project aren't linked.",
	"No relitigation history: whether this ruling was later revisited or reversed isn't tracked in the public graph.",
	"Sources are cited by GitHub ref, not dated — recency isn't shown.",
}
