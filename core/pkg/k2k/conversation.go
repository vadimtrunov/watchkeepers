package k2k

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Status is the closed-set vocabulary of states a `k2k_conversations`
// row can occupy. Hoisted to typed string constants so the in-memory
// store, the Postgres adapter, and the SQL CHECK constraint share a
// single source of truth.
//
// Project convention: snake_case strings on the wire and in payloads.
// Mirrors `saga.SagaState`'s shape (see
// `core/pkg/spawn/saga/dao.go`) so a future audit subscriber
// decoding a stored row uses the same validator pattern.
type Status string

// Closed-set values for [Status]. Matches the SQL CHECK constraint in
// `deploy/migrations/029_k2k_conversations.sql`.
const (
	// StatusOpen is the initial state set by [Repository.Open]. The
	// conversation is live and accepts [Repository.IncTokens] writes.
	StatusOpen Status = "open"

	// StatusArchived is the terminal state set by [Repository.Close].
	// The conversation no longer accepts [Repository.IncTokens] writes
	// and is filtered out of the default M1.1.c "active conversations"
	// listing.
	StatusArchived Status = "archived"
)

// Validate reports whether `s` is in the closed [Status] set. Returns
// [ErrInvalidStatus] otherwise (including the empty string). Mirrors
// `approval.DecisionKind.Validate` from `core/pkg/approval/proposalstore.go`.
func (s Status) Validate() error {
	switch s {
	case StatusOpen, StatusArchived:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidStatus, string(s))
	}
}

// Conversation is the projection of a single `k2k_conversations` row
// returned by the [Repository] surface. All fields are typed (no
// `interface{}`); reference-typed fields (the `Participants` slice) are
// defensively deep-copied on every read so a mutating caller cannot
// race the held record.
//
// The struct intentionally carries every column from the migration so a
// future M1.4 audit emitter / M1.1.c lifecycle wiring can read the
// persisted state without re-querying. Field order matches the
// migration column order for review-time alignment.
type Conversation struct {
	// ID is the row's primary key, minted by [Repository.Open] (the
	// repository, not the caller, is the source of truth for ids so
	// concurrent Opens never race on a caller-supplied UUID). Returned
	// from [Repository.Open] on the insert path.
	ID uuid.UUID

	// OrganizationID is the tenant key for the row. The matching
	// migration's RLS policies match against this column via the
	// `watchkeeper.org` session GUC; per-tenant isolation lives at the
	// Postgres layer in production and is mirrored by the in-memory
	// store's filter discipline.
	OrganizationID uuid.UUID

	// SlackChannelID is the resolved private Slack channel id for the
	// conversation. NULL / empty when the row was just opened — the
	// M1.1.c lifecycle wiring writes the channel id after the M1.1.b
	// `CreateChannel` call returns. The domain stores whatever the
	// caller hands in; no validation at this layer.
	SlackChannelID string

	// Participants is the closed set of bot ids invited to the
	// conversation's Slack channel. The repository defensively copies
	// this slice on every Open / Get / List boundary so caller-side
	// mutation cannot bleed into the held row.
	Participants []string

	// Subject is the operator-supplied free-text label for the
	// conversation. Used by M1.4 audit emission and M1.1.b channel-name
	// derivation. Required (non-empty after whitespace-trim) on Open
	// per [ErrEmptySubject].
	Subject string

	// Status is the row's current state. One of the [Status]
	// constants. Initial state is [StatusOpen]; terminal state is
	// [StatusArchived] (set by [Repository.Close]).
	Status Status

	// TokenBudget is the per-conversation token cap the M1.5 budget
	// enforcement layer consults. Zero disables enforcement. Non-zero
	// values are enforced by M1.5; the [Repository] only persists the
	// budget and the running counter.
	TokenBudget int64

	// TokensUsed is the running count of tokens consumed by the
	// conversation, incremented by [Repository.IncTokens]. Monotonic
	// per the M1.5 contract — see [ErrInvalidTokenDelta].
	TokensUsed int64

	// OpenedAt is the wall-clock time the row was inserted. Stamped
	// by the repository at Open time.
	OpenedAt time.Time

	// ClosedAt is the wall-clock time the row was archived. Zero
	// value (`time.Time{}`) while the conversation is still open.
	// Stamped by [Repository.Close].
	ClosedAt time.Time

	// CorrelationID is the optional id linking this conversation to an
	// upstream saga / Watch Order. `uuid.Nil` when the caller did not
	// supply one. Type matches the matching SQL column
	// (`correlation_id uuid NULL`); mirrors the
	// `keepers_log.correlation_id` partial-index pattern from
	// `deploy/migrations/003_keepers_log.sql`.
	CorrelationID uuid.UUID

	// CloseReason is the operator-supplied free-text rationale for the
	// archive event. Empty while the conversation is still open;
	// populated by [Repository.Close].
	CloseReason string
}

// cloneConversation defensively deep-copies the reference-typed fields
// on a [Conversation] — currently only `Participants`. Strings are
// immutable in Go (header-plus-pointer values) so aliasing the backing
// bytes is safe; the deep copy targets only the participant-slice
// header. Mirrors `cloneProposal` from
// `core/pkg/approval/proposalstore.go`.
func cloneConversation(c Conversation) Conversation {
	out := c
	if c.Participants != nil {
		out.Participants = make([]string, len(c.Participants))
		copy(out.Participants, c.Participants)
	}
	return out
}
