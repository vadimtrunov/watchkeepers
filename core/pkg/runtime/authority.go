package runtime

import "fmt"

// Authority-matrix value vocabulary the [Manifest.AuthorityMatrix]
// projection (see [runtime.go:105-110]) carries from the M5.5 manifest
// loader. Phase 1 ships this fixed set; canonicalisation lands when M6
// Watchmaster reads the matrix end-to-end.
const (
	// authorityValueLead means the action requires the human lead's
	// approval before the runtime may proceed.
	authorityValueLead = "lead"

	// authorityValueOperator means the action requires the platform
	// operator's approval before the runtime may proceed.
	authorityValueOperator = "operator"

	// authorityValueWatchmaster means the M6 Watchmaster meta-agent
	// authorises the action without human-in-the-loop.
	authorityValueWatchmaster = "watchmaster"

	// authorityValueSelf means the Watchkeeper authorises the action
	// itself (self-grant).
	authorityValueSelf = "self"
)

// RequiresApproval reports whether `action` needs out-of-band approval
// before the runtime executes it on behalf of the agent described by
// `m`. The decision is read off [Manifest.Autonomy] (see
// [runtime.go:33-48] for the [AutonomyLevel] constants) and
// [Manifest.AuthorityMatrix] (see [runtime.go:105-110]); the helper is
// consumer-agnostic — M6 Watchmaster, M9 tool authoring, and future
// supervised tool gates all consult it without re-implementing the
// table.
//
// Decision rules (in order):
//
//	┌───────────────────────────────┬────────────────────────────────────────────────────────────┐
//	│ Autonomy                      │ Outcome                                                    │
//	├───────────────────────────────┼────────────────────────────────────────────────────────────┤
//	│ "" (empty) or                 │ (true, "supervised autonomy requires per-action approval") │
//	│ AutonomySupervised            │ unconditional — action / matrix not consulted              │
//	├───────────────────────────────┼────────────────────────────────────────────────────────────┤
//	│ AutonomyAutonomous            │ map lookup on m.AuthorityMatrix[action]:                   │
//	│                               │   absent           → (false, "")                           │
//	│                               │   "self" /         │                                       │
//	│                               │   "watchmaster"    → (false, "")                           │
//	│                               │   "lead" /         │                                       │
//	│                               │   "operator"       → (true, "authority matrix requires     │
//	│                               │                       <role> approval for <action>")       │
//	│                               │   any other value  → (true, "unknown authority value       │
//	│                               │                       \"<value>\"; failing closed")        │
//	└───────────────────────────────┴────────────────────────────────────────────────────────────┘
//
// Reason-string contract: deny branches always return a non-empty
// human-readable reason suitable for downstream logging / Keeper's-Log
// emission (M3.6, M6); allow branches always return the empty string
// so callers can short-circuit on `if required { ... }`.
//
// Defensive default: an unrecognised matrix value (outside the Phase-1
// vocabulary {"self", "watchmaster", "lead", "operator"}) fails
// closed — the runtime treats it as a deny so a typo or future-vocab
// drift cannot silently widen the agent's authority.
//
// AutonomyManual is intentionally NOT a special case here — manual mode
// blocks every tool call upstream in the runtime's per-call ack flow,
// so it never reaches this helper. Treating an unrecognised
// AutonomyLevel as supervised (the default fallthrough) is the safe
// choice for forward compatibility.
func RequiresApproval(m Manifest, action string) (required bool, reason string) {
	switch m.Autonomy {
	case AutonomyAutonomous:
		// fall through to matrix lookup below.
	default:
		// Empty Autonomy, AutonomySupervised, AutonomyManual, and any
		// future-unknown level all collapse to "supervised" semantics:
		// every action requires approval.
		return true, "supervised autonomy requires per-action approval"
	}

	value, present := m.AuthorityMatrix[action]
	if !present {
		return false, ""
	}

	switch value {
	case authorityValueSelf, authorityValueWatchmaster:
		return false, ""
	case authorityValueLead, authorityValueOperator:
		return true, fmt.Sprintf("authority matrix requires %s approval for %s", value, action)
	default:
		return true, fmt.Sprintf("unknown authority value %q; failing closed", value)
	}
}
