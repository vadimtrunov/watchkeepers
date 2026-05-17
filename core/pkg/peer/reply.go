// reply.go ships the M1.3.a `peer.Reply` companion of `peer.Ask`.
// Composes the M1.3.a `k2k.Repository.AppendMessage` seam with the
// capability-broker gate; the M1.3.a `WaitForReply` signal happens
// automatically when the in-memory adapter broadcasts on its
// reply-direction cond-var (or when the Postgres adapter's polling
// loop's next tick observes the new row).
//
// resolution order:
//
//	Reply → validate inputs → capability gate (peer:reply, per-tenant)
//	     → k2k.Repository.Get resolves the conversation + checks the
//	       row is in StatusOpen
//	     → k2k.Repository.AppendMessage(direction=reply)
//	     → return nil on success.
//
// audit discipline: this file does NOT import `keeperslog` and does
// NOT call `.Append(`. The K2K message-sent audit taxonomy is emitted
// through the M1.4 `k2k/audit.Emitter` seam (typed interface) wired
// via `Deps.Auditor` (OPTIONAL; nil-permissive so M1.3.a-era wirings
// stay valid). On a successful Reply this file emits
// `audit.EventMessageSent` (the replier-side row). The original
// requester's recipient-side `audit.EventMessageReceived` is emitted
// by the `peer.Tool.Ask` flow when its `WaitForReply` unblocks — a
// subscriber joins the two on `conversation_id` + `message_id`. A
// source-grep AC test pins the keeperslog/Append ban so a future
// contributor adding inline keeperslog calls here trips a
// fast-failing test.
//
// PII discipline: the `body` payload is treated as opaque bytes.
// Defensively deep-copied before persistence so caller-side mutation
// cannot bleed.

package peer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k/audit"
)

// ReplyParams is the closed-set input shape [Tool.Reply] accepts.
// Hoisted to a struct so a future addition (e.g. a typed
// `ResponseCode` discriminator) lands as a new field rather than a
// breaking signature change.
type ReplyParams struct {
	// ActingWatchkeeperID is the id of the watchkeeper appending the
	// reply. Used as the `SenderWatchkeeperID` on the reply message.
	// Required (non-empty after whitespace-trim); the tool fail-fasts
	// via [ErrInvalidActingWatchkeeperID] otherwise.
	ActingWatchkeeperID string

	// OrganizationID is the verified tenant the acting watchkeeper
	// belongs to. Required (non-zero); the tool fail-fasts via
	// [k2k.ErrEmptyOrganization] otherwise.
	OrganizationID uuid.UUID

	// CapabilityToken is the per-call capability token bound to scope
	// [CapabilityReply] + [OrganizationID]. Required (non-empty); the
	// tool fail-fasts via [ErrPeerCapabilityDenied] when the broker
	// rejects the token.
	CapabilityToken string

	// ConversationID identifies the conversation the reply belongs
	// to. Required (non-zero); the tool fail-fasts via
	// [ErrInvalidConversationID] otherwise.
	ConversationID uuid.UUID

	// Body is the opaque reply payload appended to the conversation
	// as a `reply`-direction [k2k.Message]. Required (non-empty); the
	// tool fail-fasts via [ErrInvalidBody] otherwise. Defensively
	// deep-copied before persistence so caller-side mutation cannot
	// bleed.
	Body []byte
}

// Reply runs the M1.3.a reply-direction primitive. See the file-level
// doc-block for the resolution order; see [ReplyParams] for the input
// shape.
//
// Failure modes:
//
//   - Validation failures surface their typed sentinel
//     ([ErrInvalidActingWatchkeeperID], [k2k.ErrEmptyOrganization],
//     [ErrInvalidConversationID], [ErrInvalidBody]).
//   - Capability-broker rejection → [ErrPeerCapabilityDenied] chained
//     with the underlying [capability.Err*] sentinel.
//   - Unknown conversation → [ErrPeerConversationNotFound] chained
//     with [k2k.ErrConversationNotFound].
//   - Archived conversation → [ErrPeerConversationClosed] chained
//     with [k2k.ErrAlreadyArchived].
//   - [k2k.Repository.AppendMessage] error → wrapped through.
//   - ctx cancellation → ctx.Err().
func (t *Tool) Reply(ctx context.Context, params ReplyParams) error {
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
	if len(params.Body) == 0 {
		return ErrInvalidBody
	}

	// Capability gate BEFORE any K2K state mutation: a failed gate
	// must not append to the conversation. Mirrors `peer.Ask`'s
	// fail-fast discipline.
	if err := t.deps.Capability.ValidateForOrg(
		ctx, params.CapabilityToken, CapabilityReply, params.OrganizationID.String(),
	); err != nil {
		return translateCapabilityError(err)
	}

	// Resolve the conversation BEFORE the AppendMessage so an
	// archived row surfaces as the dedicated sentinel rather than as
	// a generic AppendMessage failure. The Get call is also the
	// natural place to enforce per-org RLS — a cross-tenant id reads
	// as not-found via the storage layer's filter (in-memory) or
	// Postgres' RLS policy.
	conv, err := t.deps.Repository.Get(ctx, params.ConversationID)
	if err != nil {
		if errors.Is(err, k2k.ErrConversationNotFound) {
			return fmt.Errorf("%w: %w", ErrPeerConversationNotFound, err)
		}
		return fmt.Errorf("peer: reply: get conversation: %w", err)
	}
	if conv.Status != k2k.StatusOpen {
		return fmt.Errorf("%w: %s", ErrPeerConversationClosed, params.ConversationID)
	}

	// Defensive deep-copy of the reply body before persistence.
	body := make([]byte, len(params.Body))
	copy(body, params.Body)

	replyMsg, err := t.deps.Repository.AppendMessage(ctx, k2k.AppendMessageParams{
		ConversationID:      params.ConversationID,
		OrganizationID:      params.OrganizationID,
		SenderWatchkeeperID: params.ActingWatchkeeperID,
		Body:                body,
		Direction:           k2k.MessageDirectionReply,
	})
	if err != nil {
		if errors.Is(err, k2k.ErrAlreadyArchived) {
			return fmt.Errorf("%w: %w", ErrPeerConversationClosed, err)
		}
		return fmt.Errorf("peer: reply: append reply: %w", err)
	}

	// M1.4 audit emission for the reply append (sender side). The
	// recipient-side `audit.EventMessageReceived` is emitted by the
	// original requester's `Tool.Ask` when its `WaitForReply` returns
	// — a subscriber joins the two halves on `conversation_id` +
	// `message_id`. Nil Auditor is a no-op; an emit failure is logged
	// but does NOT propagate — the persisted state is the load-bearing
	// surface. Detached ctx (iter-1 codex Major fix) so a caller-side
	// cancellation arriving after AppendMessage succeeded does NOT
	// systematically drop the audit row.
	if t.deps.Auditor != nil {
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditEmitTimeout)
		_, _ = t.deps.Auditor.EmitMessageSent(auditCtx, audit.MessageSentEvent{
			MessageID:           replyMsg.ID,
			ConversationID:      params.ConversationID,
			OrganizationID:      params.OrganizationID,
			SenderWatchkeeperID: params.ActingWatchkeeperID,
			Direction:           string(k2k.MessageDirectionReply),
			CreatedAt:           replyMsg.CreatedAt,
		})
		cancel()
	}
	return nil
}
