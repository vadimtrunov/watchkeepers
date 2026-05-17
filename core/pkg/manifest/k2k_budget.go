// k2k_budget.go ships the M1.5 helper that projects the per-Watchkeeper
// K2K token-budget override out of a loaded [runtime.Manifest]. The
// override lives under
// `Manifest.ImmutableCore.CostLimits["k2k_token_budget"]` per the
// Phase 2 §M3.1 schema convention — `cost_limits` is the canonical
// bucket for spend caps, and `k2k_token_budget` is the closed-set key
// the K2K family consumes.
//
// resolution order:
//
//	K2KTokenBudget → manifest.ImmutableCore == nil → (0, false)
//	             → manifest.ImmutableCore.CostLimits == nil → (0, false)
//	             → cost_limits["k2k_token_budget"] missing → (0, false)
//	             → typed numeric coerce → (value, true)
//	             → unknown / non-numeric type → (0, false)
//
// audit discipline: this helper does not import any audit / keeperslog
// surface; it only reads from a typed manifest projection.
//
// PII discipline: the manifest is operator-supplied configuration, not
// runtime payload — no body bytes or PII flow through this helper. The
// projection is purely a type-coerce from a `map[string]any` lookup.

package manifest

import (
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// K2KTokenBudgetCostLimitsKey is the closed-set lookup key the M1.5
// helper consults under [runtime.ImmutableCore.CostLimits]. Hoisted to
// a constant so the manifest writer + the budget consumer + a future
// admin-tool validator share one source of truth (mirrors the M1.4
// `EventOverBudget` constant discipline).
//
//nolint:gosec // G101: closed-set manifest schema key, not a credential.
const K2KTokenBudgetCostLimitsKey = "k2k_token_budget"

// K2KTokenBudget projects the per-Watchkeeper K2K token-budget override
// out of the supplied [runtime.Manifest]. Returns `(budget, true)` when
// the override is present + decodable; `(0, false)` otherwise (no
// ImmutableCore, no CostLimits, missing key, non-numeric value).
//
// Coercion supports the JSON-decoded numeric types that
// [runtime.ImmutableCore.CostLimits] carries:
//
//   - `float64`: the encoding/json default for any JSON number. Cast
//     to int64 (truncates the fractional part if any — operators
//     supplying `1000.7` get 1000 because token counts are integral).
//   - `int`, `int32`, `int64`: cast verbatim. Operators that hand-
//     construct the map (test fixtures, dev-config helpers) may use
//     plain integers; the helper accepts them rather than forcing a
//     float round-trip.
//
// Negative values are clamped to zero by [budget.ResolveBudget] at the
// consumer; this helper returns the raw decoded value so the caller
// can decide on the clamping discipline (M1.5's budget package clamps
// to zero == "disable enforcement"; a future operator-facing diagnostic
// may want to surface the negative value loudly instead).
//
// Unknown types (a string, a nested map, a slice) return `(0, false)`
// silently — the manifest writer is responsible for the schema; a typed
// validator at the M3.6 self-tuning surface will reject misconfigured
// types loudly. Surfacing a typed error here would force every M1.5
// caller to translate it through their own vocabulary; the closed
// `(value, ok)` shape mirrors Go's standard map-lookup idiom and keeps
// the consumer call site one-liner.
func K2KTokenBudget(mf runtime.Manifest) (int64, bool) {
	if mf.ImmutableCore == nil {
		return 0, false
	}
	if mf.ImmutableCore.CostLimits == nil {
		return 0, false
	}
	raw, present := mf.ImmutableCore.CostLimits[K2KTokenBudgetCostLimitsKey]
	if !present {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		return int64(v), true
	case int:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	default:
		return 0, false
	}
}
