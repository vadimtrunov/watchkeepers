// Package approval implements the M6.3.b
// [inbound.InteractionDispatcher] for the Watchmaster lead-approval
// saga. ROADMAP §M6 → M6.3 → M6.3.b.
//
// # Scope (M6.3.b)
//
// The dispatcher is the inbound counterpart to the M6.3.b card
// renderer (`core/pkg/messenger/slack/cards/`). It consumes
// `block_actions` payloads where the user clicked one of the two
// buttons on a Watchmaster approval card:
//
//  1. Decode the button's action_id via [cards.DecodeActionID]. The
//     format is `approval:<tool>:<token>`; a malformed value short-
//     circuits with a single `approval_card_action_received` audit
//     event carrying `reason=malformed_action_id`.
//  2. Resolve the matching `pending_approvals` row via
//     [spawn.PendingApprovalDAO.Resolve]. The `value` field on the
//     button payload (`approved` | `rejected`) is the terminal
//     decision. An unknown token surfaces as `reason=unknown_token`;
//     a row whose state is already terminal surfaces as
//     `reason=stale_state`.
//  3. On `approved`: load the row's `params_json` via
//     [spawn.PendingApprovalDAO.Get], decode it into the matching
//     M6.2.x request struct, and call the configured [Replayer] to
//     re-invoke the tool. The dispatcher emits an
//     `approval_replay_succeeded` or `approval_replay_failed` audit
//     event according to the replayer's return value. A failure in
//     the replayer DOES NOT roll back the DAO transition (per AC9 —
//     the operator should retry via a fresh approval flow).
//  4. On `rejected`: emit `approval_card_action_received` +
//     `approval_resolved` and stop.
//
// # Audit chain (per AC6 / AC9)
//
// Every dispatch emits at least one audit event. The shape of the
// chain depends on the branch taken:
//
//   - Malformed action_id: 1 event
//     (`approval_card_action_received` w/ `reason=malformed_action_id`).
//   - Unknown token: 2 events
//     (`approval_card_action_received`, `approval_resolved` w/
//     `reason=unknown_token`).
//   - Stale state: 2 events
//     (`approval_card_action_received`, `approval_resolved` w/
//     `reason=stale_state`).
//   - Reject happy path: 2 events
//     (`approval_card_action_received`, `approval_resolved` w/
//     `decision=rejected`).
//   - Approve happy path: 4 events (`approval_card_action_received`,
//     `approval_resolved` w/ `decision=approved`,
//     `approval_replay_succeeded`, plus the M6.2.x tool's own
//     `<tool>_succeeded` row emitted from the replayer).
//   - Approve + downstream tool error: 4 events (received,
//     resolved=approved, replay_failed, plus the M6.2.x tool's own
//     `<tool>_failed`).
//
// # PII discipline (per AC8)
//
// Audit payloads NEVER include `params_json` content or any field
// name from a per-tool request struct. The closed-set vocabulary the
// audit row carries:
//
//   - tool_name (one of the four spawn.PendingApprovalTool* constants
//     OR empty when the action_id was malformed before decode)
//   - approval_token (opaque hex; the mint side owns its shape)
//   - decision (approved | rejected) on the resolved event
//   - reason (closed-set: malformed_action_id, unknown_token,
//     stale_state, replay_error) on the negative branches
//   - error_class (Go type name) on `approval_replay_failed`
//
// # Non-mock test policy (per AC7)
//
// The unit-test surface uses the in-package fake DAO
// ([spawn.MemoryPendingApprovalDAO]), an inline recording fake
// replayer, and a recording fake [keeperslog.LocalKeepClient]. No
// mocking framework. This mirrors the M5.6 / M6.1.b / M6.2.x
// fakes-discipline lesson.
//
// # Stdlib-only
//
// The package depends only on encoding/json, the Go stdlib, and the
// in-repo cards / inbound / spawn / keeperslog packages.
package approval
