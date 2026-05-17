package k2k

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// MessageDirection is the closed-set discriminator on a [Message] row.
// Mirrors the SQL `CHECK (direction IN ('request', 'reply'))` constraint
// in `deploy/migrations/030_k2k_messages.sql` so the in-memory store,
// the Postgres adapter, and the schema-level invariant share a single
// source of truth.
//
// Project convention: snake_case strings on the wire and in payloads,
// matching [Status]'s shape.
type MessageDirection string

// Closed-set values for [MessageDirection].
const (
	// MessageDirectionRequest is the direction stamped on rows appended
	// by `peer.Ask` (and by future fire-and-forget `peer.Broadcast`
	// fan-out). The waiting `peer.Ask` ignores `request`-direction rows
	// when it polls / cond-var-waits for the matching reply.
	MessageDirectionRequest MessageDirection = "request"

	// MessageDirectionReply is the direction stamped on rows appended
	// by `peer.Reply`. The matching `peer.Ask` unblocks on the first
	// `reply`-direction row whose `created_at` strictly exceeds the
	// caller's `since` cursor.
	MessageDirectionReply MessageDirection = "reply"
)

// Validate reports whether `d` is in the closed [MessageDirection] set.
// Returns [ErrInvalidMessageDirection] otherwise (including the empty
// string). Mirrors [Status.Validate].
func (d MessageDirection) Validate() error {
	switch d {
	case MessageDirectionRequest, MessageDirectionReply:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidMessageDirection, string(d))
	}
}

// Message is the projection of a single `k2k_messages` row returned by
// the [Repository.AppendMessage] / [Repository.WaitForReply] surface.
// The `Body` field is `[]byte` so the M1.3.a peer-tool layer can ship
// opaque payloads (text, JSON, binary) without forcing a string round-
// trip through the storage layer. Reference-typed fields are defensively
// deep-copied on every read so a mutating caller cannot race the held
// record.
type Message struct {
	// ID is the row's primary key, minted by [Repository.AppendMessage]
	// (the repository, not the caller, is the source of truth for ids
	// so concurrent Appends never race on a caller-supplied UUID).
	ID uuid.UUID

	// ConversationID is the FK back to the parent K2K conversation. The
	// matching migration's RLS policies match against
	// `organization_id`; the FK constraint guarantees that a
	// `(conversation_id, organization_id)` pair always identifies a
	// single conversation row.
	ConversationID uuid.UUID

	// OrganizationID is the denormalised tenant key the migration's RLS
	// policy matches against (mirroring `keepers_log.organization_id`).
	OrganizationID uuid.UUID

	// SenderWatchkeeperID is the text id of the watchkeeper that
	// authored the message. Stored as text (not a uuid FK) so a fake /
	// harness participant id (matching the
	// `k2k_conversations.participants text[]` shape) round-trips
	// without a FK violation.
	SenderWatchkeeperID string

	// Body is the opaque message payload, defensively deep-copied at
	// every boundary. The peer-tool layer treats this as opaque bytes;
	// future M1.4 audit emission may pin a redaction contract.
	Body []byte

	// Direction is the closed-set discriminator from the
	// [MessageDirection] enum.
	Direction MessageDirection

	// CreatedAt is the wall-clock time the row was inserted. Stamped by
	// the repository at append time; used by [Repository.WaitForReply]
	// as the cursor anchor (`since` is exclusive).
	CreatedAt time.Time
}

// AppendMessageParams is the closed-set input shape the
// [Repository.AppendMessage] surface accepts. Hoisted to a struct so a
// future addition (e.g. a `MessageType` discriminator, a
// `CorrelationID` etc.) lands as a new field rather than a breaking
// signature change.
type AppendMessageParams struct {
	// ConversationID is the parent conversation. Required (non-zero);
	// the repository fail-fasts via [ErrEmptyConversationID] otherwise.
	ConversationID uuid.UUID

	// OrganizationID is the denormalised tenant key matched by the
	// migration's RLS policy. Required (non-zero); the in-memory
	// adapter fail-fasts via [ErrEmptyOrganization] otherwise. The
	// Postgres adapter relies on RLS to fail-close on a mismatched org.
	OrganizationID uuid.UUID

	// SenderWatchkeeperID is the text id of the authoring watchkeeper.
	// Required (non-empty after whitespace-trim); the repository fail-
	// fasts via [ErrEmptySenderWatchkeeperID] otherwise.
	SenderWatchkeeperID string

	// Body is the opaque message payload. Defensively deep-copied
	// before persistence so caller-side mutation cannot bleed.
	// Required (non-empty); the repository fail-fasts via
	// [ErrEmptyMessageBody] otherwise.
	Body []byte

	// Direction is the closed-set discriminator. Required (validated
	// against the closed set via [MessageDirection.Validate]).
	Direction MessageDirection
}

// cloneMessage defensively deep-copies the reference-typed fields on a
// [Message] — the `Body` byte slice. Strings are immutable in Go
// (header-plus-pointer values) so aliasing the backing bytes is safe;
// the deep copy targets only the body slice header. Mirrors
// [cloneConversation].
func cloneMessage(m Message) Message {
	out := m
	if m.Body != nil {
		out.Body = make([]byte, len(m.Body))
		copy(out.Body, m.Body)
	}
	return out
}
