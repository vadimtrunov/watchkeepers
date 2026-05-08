// Package intent classifies free-text Slack DM messages into the
// closed-set of M6.2.x tool-driven intents the M6.3.c DM router
// supports. The parser is a deterministic keyword/regex classifier —
// no LLM, no remote call, no clock — so a fixture-table test pins
// exactly the phrasings the router accepts.
//
// Closed-set vocabulary (matches AC1 of TASK M6.3.c):
//
//   - [IntentReadList]      → call M6.2.a `list_watchkeepers`
//   - [IntentReportCost]    → call M6.2.a `report_cost`
//   - [IntentReportHealth]  → call M6.2.a `report_health`
//   - [IntentProposeSpawn]  → call M6.2.b `propose_spawn`
//   - [IntentUnknown]       → no tool call; router posts the help DM
//
// Out-of-scope tools (`adjust_personality`, `adjust_language`,
// `retire_watchkeeper`, `promote_to_keep`) intentionally fall through
// to [IntentUnknown]; M6.3 verification list pins `propose_spawn` as
// the canonical DM-triggered manifest-bump example.
//
// Determinism contract: same input → same Intent + Params across
// invocations. Casing is normalised; leading/trailing whitespace is
// trimmed; nothing else is mutated.
package intent

import (
	"regexp"
	"strings"
)

// Intent is the closed-set classifier output. Hoisted so callers
// branch on a typed value rather than a magic string.
type Intent string

// Closed-set values for [Intent]. Project convention: snake_case
// strings that match the corresponding M6.2.x tool name (or `unknown`
// for the fall-through bucket).
const (
	IntentReadList     Intent = "list_watchkeepers"
	IntentReportCost   Intent = "report_cost"
	IntentReportHealth Intent = "report_health"
	IntentProposeSpawn Intent = "propose_spawn"
	IntentUnknown      Intent = "unknown"
)

// Result is the classifier output: the typed [Intent] plus any
// extracted parameters the router threads into the matching tool
// request. Only [IntentProposeSpawn] populates Team / Role today; the
// read-only intents need no parameters (AgentID-narrowed lookups land
// in a follow-up).
type Result struct {
	// Intent is the classifier verdict. One of the closed-set values.
	Intent Intent

	// Team is the parsed team token for [IntentProposeSpawn] (e.g.
	// "backend", "growth"). Empty for other intents OR when the
	// propose-spawn phrasing did not embed a team.
	Team string

	// Role is the parsed role token for [IntentProposeSpawn] (e.g.
	// "Coordinator", "Reviewer"). Empty for other intents OR when the
	// propose-spawn phrasing did not embed a role.
	Role string
}

// Parser is the typed seam the router consults. Single method —
// classifies a single message text. Unit tests pass the production
// classifier directly; the seam exists so a future LLM-backed
// classifier (out of scope for M6.3.c) is a one-line swap.
type Parser interface {
	// Parse classifies `text` and returns the typed [Result]. Always
	// returns a non-nil Result; never errors. Empty text returns
	// [IntentUnknown].
	Parse(text string) Result
}

// keywordParser is the M6.3.c production classifier — deterministic
// keyword + regex matching against a closed phrase table.
type keywordParser struct{}

// NewParser returns the production [Parser]. Stateless and safe for
// concurrent use.
func NewParser() Parser {
	return keywordParser{}
}

// proposeSpawnPattern captures the canonical "propose [a] <Role> for
// the <Team> team" phrasing AND a few permutations operators reach
// for in practice ("spawn a Coordinator on the growth team", "create
// Reviewer for marketing"). Group 1 is the role; group 2 is the team.
//
// The pattern is intentionally narrow — false positives on this path
// trigger an audit chain + DAO insert, so we'd rather miss a phrasing
// (router answers `unknown_intent` with the help DM) than smuggle a
// junk propose into the saga.
var proposeSpawnPattern = regexp.MustCompile(
	`(?i)\b(?:propose|spawn|create|draft)\b\s+(?:a|an|the)?\s*([a-z][a-z0-9_-]*)\s+(?:for|on|in)\s+(?:the\s+)?([a-z][a-z0-9_-]*)\s+team\b`,
)

// proposeSpawnFallback covers the no-team-suffix phrasing
// ("propose a Coordinator for backend"). Same group order.
var proposeSpawnFallback = regexp.MustCompile(
	`(?i)\b(?:propose|spawn|create|draft)\b\s+(?:a|an|the)?\s*([a-z][a-z0-9_-]*)\s+(?:for|on|in)\s+(?:the\s+)?([a-z][a-z0-9_-]*)\b`,
)

// Parse satisfies [Parser]. Resolution order:
//
//  1. Trim + lowercase the input.
//  2. Empty → [IntentUnknown].
//  3. Test the propose-spawn pattern first (most specific).
//  4. Test the cost / health / list keyword buckets.
//  5. Fall through to [IntentUnknown].
func (keywordParser) Parse(text string) Result {
	normalised := strings.TrimSpace(text)
	if normalised == "" {
		return Result{Intent: IntentUnknown}
	}
	lower := strings.ToLower(normalised)

	// Propose-spawn first — most specific phrasing, must win over
	// the read-only buckets when an admin types e.g. "propose a
	// Coordinator for the growth team to track costs".
	if m := proposeSpawnPattern.FindStringSubmatch(lower); m != nil {
		return Result{
			Intent: IntentProposeSpawn,
			Role:   m[1],
			Team:   m[2],
		}
	}
	if m := proposeSpawnFallback.FindStringSubmatch(lower); m != nil {
		return Result{
			Intent: IntentProposeSpawn,
			Role:   m[1],
			Team:   m[2],
		}
	}

	// Cost-report bucket. Phrases that mention "cost" / "spend" /
	// "tokens" / "billing".
	if matchAny(lower, costKeywords) {
		return Result{Intent: IntentReportCost}
	}

	// Health bucket. Phrases that mention "health" / "status" /
	// "alive". Listed BEFORE the list bucket so "what's the status"
	// goes to health rather than list.
	if matchAny(lower, healthKeywords) {
		return Result{Intent: IntentReportHealth}
	}

	// List bucket. Phrases that mention "running" / "list" / "show
	// watchkeepers" / "what's up".
	if matchAny(lower, listKeywords) {
		return Result{Intent: IntentReadList}
	}

	return Result{Intent: IntentUnknown}
}

// matchAny reports whether `s` contains any of the substrings in
// `needles`. Case-insensitive — the caller passes a pre-lowered
// string.
func matchAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// costKeywords / healthKeywords / listKeywords are the closed-set
// substring matchers. Hoisted to package vars so a future re-key
// (e.g., adding "expense" to the cost bucket) is a one-line change
// here that the fixture-table test picks up.
var (
	costKeywords = []string{
		"cost",
		"spend",
		"billing",
		"tokens used",
		"how much",
	}
	healthKeywords = []string{
		"health",
		"status",
		"alive",
		"heartbeat",
		"how are",
	}
	listKeywords = []string{
		"what's running",
		"whats running",
		"what is running",
		"list watchkeepers",
		"list keepers",
		"show watchkeepers",
		"show keepers",
		"who is running",
		"who's running",
		"running watchkeepers",
	}
)
