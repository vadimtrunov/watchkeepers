// ask.go ships the M1.3.a `peer.Ask` request-reply primitive. Composes
// the M1.2 `keepclient.ListPeers` target resolver, the M1.1.c
// `k2k.Lifecycle.Open` conversation minter, and the M1.3.a
// `k2k.Repository.AppendMessage` + `k2k.Repository.WaitForReply`
// message-side seams.
//
// resolution order:
//
//	Ask → validate inputs → capability gate (peer:ask, per-tenant)
//	    → resolve target (uuid first, role second) →
//	      k2k.Lifecycle.Open mints conversation + Slack channel →
//	      AppendMessage(direction=request) → WaitForReply(since=opened_at)
//	    → return (conversation_id, reply_body, nil) on success
//	      OR (uuid.Nil, nil, ErrPeerTimeout) on timeout
//	      OR (uuid.Nil, nil, ErrPeerNotFound) on unknown target
//	      OR (uuid.Nil, nil, ErrPeerCapabilityDenied) on capability deny.
//
// audit discipline: this file does NOT import `keeperslog` and does
// NOT call `.Append(`. The K2K message-sent / conversation-opened /
// message-received audit taxonomy is owned by the M1.4
// `k2k/audit.Emitter` seam — typed interface composed via
// `Deps.Auditor` (an OPTIONAL dep; nil-permissive so M1.3.a-era
// wirings stay valid). On a successful Ask the file emits
// `audit.EventMessageSent` for the request append and
// `audit.EventMessageReceived` for the observed reply; the
// `audit.EventConversationOpened` row is emitted by the M1.1.c
// lifecycle layer (the row+channel ownership lives there). A
// source-grep AC test pins the keeperslog/Append ban so a future
// contributor adding inline keeperslog calls here trips a
// fast-failing test.
//
// PII discipline: the `body` payload is treated as opaque bytes. The
// implementation defensively deep-copies the input + the result so
// caller-side mutation cannot bleed in either direction. No payload
// bytes ever reach the returned error or any diagnostic surface.

package peer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k/audit"
)

// AskParams is the closed-set input shape [Tool.Ask] accepts. Hoisted
// to a struct (rather than a long positional arg list) so a future
// addition (e.g. an explicit `CorrelationID` for the M1.6 escalation
// saga) lands as a new field rather than a breaking signature change.
type AskParams struct {
	// ActingWatchkeeperID is the id of the watchkeeper invoking the
	// tool. Used as the `SenderWatchkeeperID` on the request message
	// and as a participant on the minted conversation. Required
	// (non-empty after whitespace-trim); the tool fail-fasts via
	// [ErrInvalidActingWatchkeeperID] otherwise.
	ActingWatchkeeperID string

	// OrganizationID is the verified tenant the acting watchkeeper
	// belongs to — typically carried on `auth.Claim.OrganizationID`
	// from the keep HTTP layer. Required (non-zero); the tool fail-
	// fasts via [k2k.ErrEmptyOrganization] otherwise. Used to gate the
	// capability broker AND to scope the persisted K2K conversation.
	OrganizationID uuid.UUID

	// CapabilityToken is the per-call capability token bound to scope
	// [CapabilityAsk] + [OrganizationID] via
	// [capability.Broker.IssueForOrg]. The tool validates it via
	// [capability.Broker.ValidateForOrg] BEFORE any K2K state
	// mutation. Required (non-empty); the tool fail-fasts via
	// [ErrPeerCapabilityDenied] when the broker rejects the token.
	CapabilityToken string

	// Target identifies the peer to ask — either a watchkeeper id
	// (matched against `keepclient.Peer.WatchkeeperID`) or a
	// case-insensitive role name (matched against
	// `keepclient.Peer.Role`). Required (non-empty after
	// whitespace-trim); the tool fail-fasts via [ErrInvalidTarget]
	// otherwise.
	Target string

	// Subject is the operator-facing free-text label persisted onto
	// the minted [k2k.Conversation.Subject]. Required (non-empty
	// after whitespace-trim); the tool fail-fasts via
	// [ErrInvalidSubject] otherwise.
	Subject string

	// Body is the opaque request payload appended to the conversation
	// as a `request`-direction [k2k.Message]. Required (non-empty);
	// the tool fail-fasts via [ErrInvalidBody] otherwise. Defensively
	// deep-copied before persistence so caller-side mutation cannot
	// bleed.
	Body []byte

	// Timeout caps the WaitForReply blocking window. Required
	// (positive); the tool fail-fasts via [ErrInvalidTimeout] for any
	// non-positive value. The M1.3.d `peer.Broadcast` fire-and-forget
	// path is owned by that leaf — `Ask` itself always blocks.
	Timeout time.Duration

	// CorrelationID is an optional id linking the conversation to an
	// upstream saga / Watch Order. `uuid.Nil` when the caller has
	// nothing to correlate. Forwarded verbatim to
	// [k2k.OpenParams.CorrelationID].
	CorrelationID uuid.UUID
}

// AskResult is the closed-set output shape [Tool.Ask] returns.
// Hoisted to a struct (rather than three positional returns) so a
// future addition (e.g. the reply message's `CreatedAt` timestamp)
// lands as a new field rather than a breaking signature change.
type AskResult struct {
	// ConversationID is the id of the K2K conversation minted to
	// carry this ask. Persisted in `k2k_conversations` and available
	// via [k2k.Repository.Get] for the duration of the
	// conversation's lifecycle.
	ConversationID uuid.UUID

	// ReplyBody is the opaque reply payload from the matching
	// `peer.Reply` call. Defensively deep-copied; mutating the slice
	// does not affect the persisted row.
	ReplyBody []byte
}

// Ask runs the M1.3.a request-reply primitive. See the file-level
// doc-block for the resolution order; see [AskParams] for the input
// shape and [AskResult] for the output shape.
//
// Failure modes:
//
//   - Validation failures surface their typed sentinel ([ErrInvalidTarget],
//     [ErrInvalidSubject], [ErrInvalidBody], [ErrInvalidTimeout],
//     [ErrInvalidActingWatchkeeperID], [k2k.ErrEmptyOrganization]).
//   - Capability-broker rejection → [ErrPeerCapabilityDenied] chained
//     with the underlying [capability.Err*] sentinel.
//   - Target resolution miss → [ErrPeerNotFound].
//   - [k2k.Lifecycle.Open] error → wrapped through.
//   - [k2k.Repository.AppendMessage] error → wrapped through.
//   - [k2k.Repository.WaitForReply] timeout → [ErrPeerTimeout] chained
//     with the underlying [k2k.ErrWaitForReplyTimeout] sentinel.
//   - ctx cancellation → ctx.Err().
//
// The tool does NOT attempt to close the minted conversation on
// failure — that is the caller's choice (a follow-up `peer.Close`
// from M1.3.b, or an M1.7 archive-on-summary writer). A successful
// Ask that times out still leaves the conversation row in
// [k2k.StatusOpen]; the caller may retry on the same conversation or
// archive it via `peer.Close`.
func (t *Tool) Ask(ctx context.Context, params AskParams) (AskResult, error) {
	if err := ctx.Err(); err != nil {
		return AskResult{}, err
	}
	if strings.TrimSpace(params.ActingWatchkeeperID) == "" {
		return AskResult{}, ErrInvalidActingWatchkeeperID
	}
	if params.OrganizationID == uuid.Nil {
		return AskResult{}, k2k.ErrEmptyOrganization
	}
	if strings.TrimSpace(params.Target) == "" {
		return AskResult{}, ErrInvalidTarget
	}
	if strings.TrimSpace(params.Subject) == "" {
		return AskResult{}, ErrInvalidSubject
	}
	if len(params.Body) == 0 {
		return AskResult{}, ErrInvalidBody
	}
	if params.Timeout <= 0 {
		return AskResult{}, ErrInvalidTimeout
	}

	// Capability gate BEFORE any K2K state mutation: a failed gate
	// must not mint a conversation row OR a Slack channel. Mirrors
	// the M1.1.c "fail-fast precedes Slack" discipline at the
	// peer-tool layer.
	if err := t.deps.Capability.ValidateForOrg(
		ctx, params.CapabilityToken, CapabilityAsk, params.OrganizationID.String(),
	); err != nil {
		return AskResult{}, translateCapabilityError(err)
	}

	// Resolve target. We do this BEFORE k2k.Lifecycle.Open so an
	// unknown target does not mint a conversation row that has to be
	// reaped later. Mirrors the "fail-fast precedes persistence"
	// discipline from M7.1.b.
	peer, err := t.resolvePeer(ctx, params.Target)
	if err != nil {
		return AskResult{}, err
	}

	// Defensive deep-copy of the request body — both for the
	// [k2k.AppendMessageParams] argument and for any future use by
	// this function — so a caller mutating the slice after Ask
	// returns cannot bleed.
	reqBody := make([]byte, len(params.Body))
	copy(reqBody, params.Body)

	// Mint the conversation. The acting watchkeeper id is included
	// as a participant alongside the resolved peer so the
	// conversation's `participants` text[] reflects the membership.
	conv, err := t.deps.Lifecycle.Open(ctx, k2k.OpenParams{
		OrganizationID: params.OrganizationID,
		Participants:   []string{params.ActingWatchkeeperID, peer.WatchkeeperID},
		Subject:        params.Subject,
		CorrelationID:  params.CorrelationID,
	})
	if err != nil {
		return AskResult{}, fmt.Errorf("peer: ask: open conversation: %w", err)
	}

	// Capture the wall-clock BEFORE the AppendMessage so the
	// WaitForReply `since` cursor is strictly earlier than the
	// reply's `created_at`. The cursor is exclusive on the storage
	// side; using a pre-append timestamp here guarantees a reply
	// stamped at exactly the AppendMessage's `created_at` would NOT
	// satisfy the wait (which is the correct semantic — the request
	// is not its own reply).
	since := t.now().UTC()

	reqMsg, err := t.deps.Repository.AppendMessage(ctx, k2k.AppendMessageParams{
		ConversationID:      conv.ID,
		OrganizationID:      params.OrganizationID,
		SenderWatchkeeperID: params.ActingWatchkeeperID,
		Body:                reqBody,
		Direction:           k2k.MessageDirectionRequest,
	})
	if err != nil {
		return AskResult{}, fmt.Errorf("peer: ask: append request: %w", err)
	}

	// M1.4 audit emission for the request append. Nil Auditor is a
	// no-op; an emit failure is logged but does NOT propagate — the
	// peer-tool surface is gated on persisted state, not observability.
	if t.deps.Auditor != nil {
		_, _ = t.deps.Auditor.EmitMessageSent(ctx, audit.MessageSentEvent{
			MessageID:           reqMsg.ID,
			ConversationID:      conv.ID,
			OrganizationID:      params.OrganizationID,
			SenderWatchkeeperID: params.ActingWatchkeeperID,
			Direction:           string(k2k.MessageDirectionRequest),
			CreatedAt:           reqMsg.CreatedAt,
		})
	}

	reply, err := t.deps.Repository.WaitForReply(ctx, conv.ID, since, params.Timeout)
	if err != nil {
		if errors.Is(err, k2k.ErrWaitForReplyTimeout) {
			return AskResult{}, fmt.Errorf("%w: %w", ErrPeerTimeout, err)
		}
		return AskResult{}, fmt.Errorf("peer: ask: wait for reply: %w", err)
	}

	// M1.4 audit emission for the observed reply (receiver side). The
	// replier's `peer.Reply` emits the sender side; this emit captures
	// the round-trip's recipient view so a subscriber can join the two
	// halves on `conversation_id` + `message_id`.
	if t.deps.Auditor != nil {
		_, _ = t.deps.Auditor.EmitMessageReceived(ctx, audit.MessageReceivedEvent{
			MessageID:              reply.ID,
			ConversationID:         conv.ID,
			OrganizationID:         params.OrganizationID,
			SenderWatchkeeperID:    reply.SenderWatchkeeperID,
			RecipientWatchkeeperID: params.ActingWatchkeeperID,
			Direction:              string(k2k.MessageDirectionReply),
			CreatedAt:              reply.CreatedAt,
		})
	}

	// Defensive deep-copy on the way out so the caller cannot bleed
	// into the storage layer's held value (the storage layer already
	// returned a copy, but the contract here documents the
	// boundary).
	out := make([]byte, len(reply.Body))
	copy(out, reply.Body)
	return AskResult{ConversationID: conv.ID, ReplyBody: out}, nil
}
