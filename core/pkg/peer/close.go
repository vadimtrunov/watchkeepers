// close.go ships the M1.3.b `peer.Close` lifecycle-finalize primitive.
// Composes the M1.1.c `k2k.Lifecycle.Close` (archives the bound Slack
// channel + transitions `k2k_conversations.status` to `archived`) with
// a one-line operator-facing summary write via the new M1.3.b
// `k2k.Repository.SetCloseSummary` surface (migration
// `031_k2k_close_summary.sql`).
//
// resolution order:
//
//	Close → ctx.Err → validate inputs → capability gate (peer:close,
//	     per-tenant) → k2k.Repository.Get (existence + participant gate
//	     using the acting watchkeeper's id against
//	     `Conversation.Participants`) → k2k.Lifecycle.Close(ctx, id,
//	     reason="peer.close") → k2k.Repository.SetCloseSummary(ctx, id,
//	     summary) → return nil on success.
//
// Idempotent double-close: a second `peer.Close` on a row already in
// `StatusArchived` is a no-op returning nil. The flow detects this in
// two places — a Get that observes `StatusArchived` skips both
// Lifecycle.Close and SetCloseSummary; a race where the Get observes
// StatusOpen but Lifecycle.Close surfaces ErrAlreadyArchived also
// returns nil (the saga-replay path). This mirrors the M1.1.b
// `ArchiveChannel` idempotent-on-`already_archived` discipline and the
// M1.1.c lifecycle's [k2k.ErrAlreadyArchived] translation. Critically,
// an idempotent no-op MUST NOT overwrite an existing `close_summary`
// because the first close's summary is the load-bearing one.
//
// audit discipline: this file does NOT import `keeperslog` and does
// NOT call `.Append(`. The K2K `k2k_conversation_closed` audit
// taxonomy is owned by the M1.4 audit subscriber; this file is the
// call surface, not the audit sink. A source-grep AC test pins this
// so a future contributor adding inline audit emission here trips a
// fast-failing test.
//
// PII discipline: the `summary` payload is operator-supplied free
// text. The implementation forwards it verbatim to
// `Repository.SetCloseSummary` (an immutable Go string — no defensive
// copy required). The acting watchkeeper id is load-bearing for the
// participant-membership gate; an empty value short-circuits via
// [ErrInvalidActingWatchkeeperID] before the capability gate.

package peer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
)

// CloseLifecycleReason is the stable reason code [Tool.Close] passes
// to [k2k.Lifecycle.Close]'s `reason` arg. Hoisted to a const so the
// M1.7 archive-on-summary writer (and any future M1.4 audit subscriber
// joining on `close_reason`) can identify peer-tool-driven closes
// without parsing the column value. The M1.6 escalation auto-archive
// uses its own distinct code.
const CloseLifecycleReason = "peer.close"

// CloseParams is the closed-set input shape [Tool.Close] accepts.
// Hoisted to a struct (rather than a long positional arg list) so a
// future addition (e.g. an explicit `Visibility` field for the M1.7
// archive-on-summary writer) lands as a new field rather than a
// breaking signature change. Mirrors the [AskParams] / [ReplyParams]
// discipline.
type CloseParams struct {
	// ActingWatchkeeperID is the id of the watchkeeper invoking the
	// close. Required (non-empty after whitespace-trim); the tool
	// fail-fasts via [ErrInvalidActingWatchkeeperID] otherwise. Also
	// used as the principal for the participant-membership gate — the
	// acting watchkeeper MUST appear in the conversation's
	// `Participants` slice, otherwise the call surfaces
	// [ErrPeerClosePermission].
	ActingWatchkeeperID string

	// OrganizationID is the verified tenant the acting watchkeeper
	// belongs to. Required (non-zero); the tool fail-fasts via
	// [k2k.ErrEmptyOrganization] otherwise. Used to gate the capability
	// broker; the K2K Repository.Get path already scopes by tenant via
	// the RLS GUC (Postgres) or filter (in-memory).
	OrganizationID uuid.UUID

	// CapabilityToken is the per-call capability token bound to scope
	// [CapabilityClose] + [OrganizationID]. Required (non-empty); the
	// tool fail-fasts via [ErrPeerCapabilityDenied] when the broker
	// rejects the token.
	CapabilityToken string

	// ConversationID identifies the conversation to close. Required
	// (non-zero); the tool fail-fasts via [ErrInvalidConversationID]
	// otherwise.
	ConversationID uuid.UUID

	// Summary is the operator-supplied one-line free-text summary
	// persisted to `k2k_conversations.close_summary`. Optional — an
	// empty / whitespace-only value records an empty summary. The
	// summary is forwarded verbatim to
	// [k2k.Repository.SetCloseSummary]; no defensive copy is required
	// (Go strings are immutable).
	Summary string
}

// CapabilityClose is the capability scope [Tool.Close] gates against.
// Mirrors [CapabilityAsk] / [CapabilityReply]; the acting agent's
// Manifest must declare the capability under its `capabilities:` block
// and the runtime mints a per-call token scoped to this string via
// [capability.Broker.IssueForOrg].
const CapabilityClose = "peer:close"

// Close runs the M1.3.b lifecycle-finalize primitive. See the
// file-level doc-block for the resolution order; see [CloseParams] for
// the input shape.
//
// Failure modes:
//
//   - Validation failures surface their typed sentinel
//     ([ErrInvalidActingWatchkeeperID], [k2k.ErrEmptyOrganization],
//     [ErrInvalidConversationID]).
//   - Capability-broker rejection → [ErrPeerCapabilityDenied] chained
//     with the underlying [capability.Err*] sentinel.
//   - Unknown conversation → [ErrPeerConversationNotFound] chained
//     with [k2k.ErrConversationNotFound].
//   - Acting watchkeeper not a participant → [ErrPeerClosePermission].
//   - [k2k.Lifecycle.Close] error other than [k2k.ErrAlreadyArchived]
//     → wrapped through.
//   - [k2k.Repository.SetCloseSummary] error → wrapped through.
//   - ctx cancellation → ctx.Err().
//
// Idempotent close: returns nil on a second close attempt against a
// row already in [k2k.StatusArchived]. The flow short-circuits when
// the initial [k2k.Repository.Get] observes the archived row; a race
// where Get sees open and Lifecycle.Close races to archive first
// surfaces [k2k.ErrAlreadyArchived] which is also translated to nil.
// On the idempotent path the existing `close_summary` is preserved —
// the FIRST close's summary is load-bearing for the M1.7
// archive-on-summary writer; a second close MUST NOT overwrite it.
func (t *Tool) Close(ctx context.Context, params CloseParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if strings.TrimSpace(params.ActingWatchkeeperID) == "" {
		return ErrInvalidActingWatchkeeperID
	}
	if params.OrganizationID == uuid.Nil {
		return k2k.ErrEmptyOrganization
	}
	if params.ConversationID == uuid.Nil {
		return ErrInvalidConversationID
	}

	// Capability gate BEFORE any K2K state mutation: a failed gate must
	// not archive the channel or write a summary. Mirrors `peer.Ask` /
	// `peer.Reply`'s fail-fast discipline.
	if err := t.deps.Capability.ValidateForOrg(
		ctx, params.CapabilityToken, CapabilityClose, params.OrganizationID.String(),
	); err != nil {
		return translateCapabilityError(err)
	}

	// Resolve the conversation BEFORE Lifecycle.Close so:
	//   1. Unknown conversation surfaces the dedicated sentinel rather
	//      than a generic Lifecycle.Close failure.
	//   2. The cross-tenant defensive check + participant-membership
	//      gate run over the persisted row before any side effect.
	//   3. An already-archived row short-circuits both Lifecycle.Close
	//      AND (when its existing close_summary is non-empty)
	//      SetCloseSummary so the idempotent no-op preserves the
	//      original close_summary while still allowing recovery of a
	//      half-completed close (archive succeeded, summary write
	//      failed) on a subsequent retry.
	conv, err := t.deps.Repository.Get(ctx, params.ConversationID)
	if err != nil {
		if errors.Is(err, k2k.ErrConversationNotFound) {
			return fmt.Errorf("%w: %w", ErrPeerConversationNotFound, err)
		}
		return fmt.Errorf("peer: close: get conversation: %w", err)
	}

	// Cross-tenant defensive check. Production Postgres wiring already
	// filters by tenant via the `watchkeeper.org` RLS GUC (an unset /
	// mismatched GUC fail-closes to ErrConversationNotFound); this
	// in-process check belongs alongside the capability gate so the
	// in-memory adapter + any future non-RLS-aware Repository impl
	// honour the same boundary. The mismatch surfaces as
	// ErrPeerConversationNotFound (not a leaky cross-tenant existence
	// signal) so the caller cannot probe for foreign-tenant ids by
	// observing a different error.
	if conv.OrganizationID != params.OrganizationID {
		return fmt.Errorf("%w: %s", ErrPeerConversationNotFound, params.ConversationID)
	}

	// Participant-membership gate. The acting watchkeeper must appear
	// in the conversation's participants slice. The gate runs over an
	// exact (case-sensitive) match because participant ids are minted
	// by the platform (not free-text); a case-insensitive match would
	// admit a typo'd id that happens to differ only in case. Surfaces
	// [ErrPeerClosePermission] WITHOUT leaking the participant list in
	// the error message — the diagnostic chain carries the conversation
	// id only.
	if !participantMatches(conv.Participants, params.ActingWatchkeeperID) {
		return fmt.Errorf("%w: %s", ErrPeerClosePermission, params.ConversationID)
	}

	// Idempotent double-close short-circuit BEFORE Lifecycle.Close.
	// Two sub-cases:
	//   * Archived row with a non-empty CloseSummary — fully complete.
	//     Return nil without touching Lifecycle.Close OR
	//     SetCloseSummary so the first close's summary persists.
	//   * Archived row with an empty CloseSummary — the prior close
	//     archived the row but failed to write the summary (e.g. a
	//     Postgres tx aborted after the archive UPDATE committed but
	//     before SetCloseSummary). Skip Lifecycle.Close (already done)
	//     but DO call SetCloseSummary so the caller's summary lands on
	//     the row. This recovers a half-completed close without
	//     forcing a manual reaper. Mirrors the M1.1.c orphan-row
	//     recovery discipline.
	if conv.Status == k2k.StatusArchived {
		if conv.CloseSummary != "" {
			return nil
		}
		if err := t.deps.Repository.SetCloseSummary(ctx, params.ConversationID, params.Summary); err != nil {
			return fmt.Errorf("peer: close: set close summary (recovery): %w", err)
		}
		return nil
	}

	// Drive the lifecycle close (archive Slack channel + transition row
	// to StatusArchived).
	if err := t.deps.Lifecycle.Close(ctx, params.ConversationID, CloseLifecycleReason); err != nil {
		// Saga-replay race: Get observed StatusOpen but Lifecycle.Close
		// lost the race to a concurrent close. Surface as nil per the
		// idempotent contract; the concurrent close's SetCloseSummary
		// owns the persisted summary.
		if errors.Is(err, k2k.ErrAlreadyArchived) {
			return nil
		}
		return fmt.Errorf("peer: close: lifecycle close: %w", err)
	}

	// Write the operator summary onto the now-archived row. A
	// successful Lifecycle.Close guarantees the row is in
	// StatusArchived, so SetCloseSummary never surfaces
	// ErrConversationNotArchived under non-racing operation.
	if err := t.deps.Repository.SetCloseSummary(ctx, params.ConversationID, params.Summary); err != nil {
		return fmt.Errorf("peer: close: set close summary: %w", err)
	}
	return nil
}

// participantMatches reports whether `actingID` appears in the
// `participants` slice. The match is case-sensitive and exact (no
// whitespace-trim of the stored ids — the M1.1.a Repository.Open
// rejects whitespace-only entries at the entry boundary, so every
// persisted id is canonical). Hoisted to a helper so the AC-defined
// participant gate has a single pinned implementation.
func participantMatches(participants []string, actingID string) bool {
	for _, p := range participants {
		if p == actingID {
			return true
		}
	}
	return false
}
