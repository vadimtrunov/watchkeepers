// pending_approval.go defines the M6.3.b approval-saga DAO surface plus
// the shared closed-set vocabulary the InteractionDispatcher (M6.3.b
// inbound/approval) consults.
//
// The DAO is interface-only here: M6.3.b owns the contract + a fake
// implementation for tests, and a production HTTP-backed impl lands in
// a follow-up (the keepclient currently does not expose a
// pending_approvals endpoint set; M6.3.c may add it, or a Postgres-
// backed adapter may live in a new package). Putting the interface in
// `core/pkg/spawn` lets M6.2.x tool entrypoints AND the M6.3.b
// dispatcher reference the same shape without an import cycle.
//
// Pre-flagged design choice (M6.3.b TASK §"Pre-flag any design
// choices"): DAO location is `core/pkg/spawn/`. Rationale:
//
//   - Symmetry with the M6.2.x tools (every Watchmaster privileged
//     write lives under `spawn`; the approval saga is the persistence
//     side of the same flow).
//   - Avoids a new package for a 3-method interface.
//   - The approval token is conceptually owned by the spawn flow
//     (every M6.2.x tool ALREADY threads `ApprovalToken` through its
//     request shape).
//
// PII discipline: `Get` returns `params_json` as-is so the dispatcher
// can re-invoke the tool, but the dispatcher's audit chain NEVER
// reflects any field name from `params_json` onto a keepers_log row
// (M2b.7 / M6.2.d). The DAO itself does not emit audit rows.
package spawn

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// PendingApprovalDecision is the closed-set vocabulary of terminal
// states a `pending_approvals` row can be resolved to. The values
// match the SQL CHECK constraint (`approved` | `rejected`) plus the
// initial state (`pending`). Hoisted so the DAO impl, the
// InteractionDispatcher, and the keepers_log payload share a single
// source of truth.
//
// Project convention: snake_case strings on the wire and in payloads.
type PendingApprovalDecision string

// Closed-set values for [PendingApprovalDecision]. Matches the SQL
// CHECK constraint in `deploy/migrations/018_pending_approvals.sql`.
const (
	PendingApprovalStatePending  PendingApprovalDecision = "pending"
	PendingApprovalStateApproved PendingApprovalDecision = "approved"
	PendingApprovalStateRejected PendingApprovalDecision = "rejected"
)

// PendingApprovalToolName is the closed-set vocabulary of tool names
// the approval saga supports in M6.3.b — the four manifest-bump tools
// from M6.2.b/c (`propose_spawn`, `adjust_personality`,
// `adjust_language`, `retire_watchkeeper`). Hoisted to constants so a
// re-key from the harness-side `builtinTools.ts` registry is a one-
// line change here that the DAO, dispatcher, and renderer pick up via
// the compiler.
//
// `promote_to_keep` (M6.2.d) is intentionally NOT in this list —
// M6.3.d explicitly owns its diff-preview rendering per the M6.3
// decomposition.
const (
	PendingApprovalToolProposeSpawn      = "propose_spawn"
	PendingApprovalToolAdjustPersonality = "adjust_personality"
	PendingApprovalToolAdjustLanguage    = "adjust_language"
	PendingApprovalToolRetireWatchkeeper = "retire_watchkeeper"
)

// ErrPendingApprovalNotFound is the typed error [PendingApprovalDAO.Get]
// and [PendingApprovalDAO.Resolve] return when the supplied
// approval_token does not match any row. Matchable via [errors.Is].
// Distinct from a generic SQL miss so the dispatcher's audit chain can
// emit `reason=unknown_token` without parsing the underlying error.
var ErrPendingApprovalNotFound = errors.New("spawn: pending approval not found")

// ErrPendingApprovalStaleState is the typed error
// [PendingApprovalDAO.Resolve] returns when the row's `state` is
// already a terminal value (i.e. another caller resolved it first).
// The DAO does NOT mutate the row in this branch; the dispatcher's
// audit chain emits `reason=stale_state` and skips the replay.
//
// Distinct from [ErrPendingApprovalNotFound] so the dispatcher can
// report the right reason: a "stale" row exists but cannot be
// re-resolved; an "unknown" row never existed.
var ErrPendingApprovalStaleState = errors.New("spawn: pending approval state is terminal")

// ErrPendingApprovalInvalidDecision is returned by
// [PendingApprovalDAO.Resolve] when the supplied decision is not one
// of [PendingApprovalStateApproved] or [PendingApprovalStateRejected].
// `pending` is the initial state, not a valid resolution target.
var ErrPendingApprovalInvalidDecision = errors.New("spawn: invalid pending approval decision")

// PendingApproval is the projection of a single `pending_approvals`
// row the DAO surface returns from [PendingApprovalDAO.Get]. Exported
// so the dispatcher can read `ToolName` + `ParamsJSON` to drive the
// replay branch.
type PendingApproval struct {
	// ApprovalToken is the row's primary key — the opaque token the
	// M6.2.x tool minted at request time and the dispatcher decoded
	// from the action_id payload.
	ApprovalToken string

	// ToolName is the closed-set vocabulary value the dispatcher
	// branches on. One of the [PendingApprovalToolName] constants.
	ToolName string

	// ParamsJSON is the full request body snapshot encoded as the
	// caller's domain-specific shape. The dispatcher unmarshals this
	// into the matching M6.2.x request struct before calling the
	// replayer; NEVER reflected on any audit row.
	ParamsJSON json.RawMessage

	// State is the row's current state. One of
	// [PendingApprovalStatePending] | [PendingApprovalStateApproved] |
	// [PendingApprovalStateRejected].
	State PendingApprovalDecision

	// RequestedAt is the wall-clock time the row was inserted; set by
	// the SQL DEFAULT now() at insert time. Populated by the DAO impl
	// from the `requested_at` column.
	RequestedAt time.Time

	// ResolvedAt is the wall-clock time the row transitioned to a
	// terminal state. Zero value (`time.Time{}`) when `State ==
	// pending`.
	ResolvedAt time.Time
}

// PendingApprovalDAO is the M6.3.b persistence seam for the approval
// saga. The interface is the unit-test seam: the production impl will
// be a Postgres-backed adapter (deferred to M6.3.c or a follow-up);
// the M6.3.b dispatcher tests use the in-package fake defined in
// `pending_approval_fake_test.go`.
//
// All methods are safe for concurrent use across goroutines on the
// same DAO value; per-call state lives on the goroutine stack.
//
// Resolution discipline: `Insert` is idempotent on `approval_token`
// only insofar as a duplicate insert is the caller's bug — the DAO
// surfaces a wrapped error rather than silently swallowing. `Resolve`
// is the only mutation entry point that performs a state-machine
// check.
type PendingApprovalDAO interface {
	// Insert persists a new row with `state='pending'`, the supplied
	// `tool_name`, and the caller's `paramsJSON` snapshot. Returns a
	// wrapped error on duplicate `approval_token` (caller bug).
	//
	// `paramsJSON` is the JSON-encoded request body the M6.2.x tool
	// received; the DAO does not validate the shape (the matching
	// replayer does at re-invoke time).
	Insert(ctx context.Context, token, tool string, paramsJSON json.RawMessage) error

	// Get returns the row matching `token` or
	// [ErrPendingApprovalNotFound] when no such row exists.
	Get(ctx context.Context, token string) (*PendingApproval, error)

	// Resolve transitions a `pending` row to a terminal state. Returns:
	//
	//   - [ErrPendingApprovalNotFound] when no row matches `token`.
	//   - [ErrPendingApprovalStaleState] when the row's state is
	//     already terminal (no mutation).
	//   - [ErrPendingApprovalInvalidDecision] when `decision` is not
	//     one of the two terminal values.
	//   - any underlying transport / SQL error wrapped with the DAO
	//     impl's package prefix.
	//
	// On success, `state` flips to `decision` and `resolved_at` is
	// stamped to the wall-clock moment of the transition.
	Resolve(ctx context.Context, token string, decision PendingApprovalDecision) error
}
