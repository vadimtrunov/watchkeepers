package k2k

import (
	"context"

	"github.com/google/uuid"
)

// ListFilter is the closed-set query shape the [Repository.List]
// surface accepts. A zero value selects every row visible under the
// caller's scope (the in-memory store ignores filters; the Postgres
// adapter relies on the per-tenant RLS policy from migration 029 to
// scope the result set).
//
// The filter is intentionally narrow at M1.1.a — the M1.1.c lifecycle
// wiring and the M1.2 `keepclient.list_peers` integration consume
// `Status` only; richer filters (participant membership, correlation
// id, time window) land alongside the consumer that needs them per
// the YAGNI discipline established in M6.3.b.
type ListFilter struct {
	// OrganizationID restricts the result set to rows under the
	// supplied tenant. Required for the in-memory adapter (it does not
	// have an ambient session GUC to fall back on); the Postgres
	// adapter ignores this field and relies on the RLS policy keyed
	// off `watchkeeper.org`.
	OrganizationID uuid.UUID

	// Status optionally restricts the result set to rows in the given
	// state. Zero value (empty string) returns every status.
	Status Status
}

// OpenParams is the closed-set input shape the [Repository.Open]
// surface accepts. Hoisted to a struct (rather than a long positional
// arg list) so a future addition (e.g. the M1.4 `correlation_id`
// argument when the kickoff saga starts threading it through) lands as
// a new field rather than a breaking signature change.
type OpenParams struct {
	// OrganizationID is the tenant the conversation belongs to.
	// Required (non-zero); the repository fail-fasts via
	// [ErrEmptyOrganization] otherwise.
	OrganizationID uuid.UUID

	// Participants is the closed set of bot ids invited to the
	// conversation. Required (non-empty, no empty / whitespace-only
	// entries); the repository fail-fasts via [ErrEmptyParticipants]
	// otherwise. The slice is defensively deep-copied before
	// persistence so caller-side mutation cannot bleed.
	Participants []string

	// Subject is the operator-supplied free-text label. Required
	// (non-empty after whitespace-trim); the repository fail-fasts via
	// [ErrEmptySubject] otherwise.
	Subject string

	// TokenBudget is the per-conversation token cap. Must be
	// non-negative; zero disables enforcement. The repository
	// fail-fasts via [ErrInvalidTokenBudget] on a negative value.
	TokenBudget int64

	// CorrelationID is an optional id linking the conversation to an
	// upstream saga / Watch Order. `uuid.Nil` when the caller has
	// nothing to correlate; type matches the matching SQL column
	// (`correlation_id uuid NULL`).
	CorrelationID uuid.UUID
}

// Repository is the persistence seam for the K2K conversation domain.
// The interface is the unit-test seam: the production impl is
// [PostgresRepository]; tests + dev / smoke loops use
// [MemoryRepository]. Mirrors the
// `saga.SpawnSagaDAO` discipline (see
// `core/pkg/spawn/saga/dao.go`): the seam is narrow, every method is
// safe for concurrent use, and resolution order is documented per
// method.
//
// Lifecycle ordering: [Open] mints a fresh row in [StatusOpen]; [Get]
// resolves by id; [List] enumerates under the supplied filter;
// [IncTokens] monotonically advances the running counter on an open
// row; [Close] transitions the row to [StatusArchived]. The lifecycle
// is strictly open→archived — a second [Close] on the same id returns
// [ErrAlreadyArchived].
type Repository interface {
	// Open persists a new row in [StatusOpen] with a freshly minted
	// id, stamps `OpenedAt`, and returns the resulting [Conversation].
	// The id is minted by the repository (not the caller) so two
	// concurrent Opens never race on a caller-supplied UUID.
	//
	// Validation order (fail-fast precedes persistence):
	//   1. ctx.Err — refuses a pre-cancelled ctx.
	//   2. params.OrganizationID != uuid.Nil — [ErrEmptyOrganization].
	//   3. trimmed params.Subject != "" — [ErrEmptySubject].
	//   4. len(params.Participants) > 0 AND no empty / whitespace-only
	//      entry — [ErrEmptyParticipants].
	//   5. params.TokenBudget >= 0 — [ErrInvalidTokenBudget].
	//
	// The returned [Conversation.Participants] slice is a defensive
	// copy; mutating it does not affect the persisted row.
	Open(ctx context.Context, params OpenParams) (Conversation, error)

	// Get resolves the row matching `id` or returns
	// [ErrConversationNotFound]. The returned [Conversation] is a
	// value copy with a defensive deep-copy of the `Participants`
	// slice; mutating it does not affect the persisted row.
	//
	// Resolution order:
	//   1. ctx.Err — refuses a pre-cancelled ctx.
	//   2. row lookup — miss surfaces [ErrConversationNotFound]
	//      wrapped with the requested id.
	Get(ctx context.Context, id uuid.UUID) (Conversation, error)

	// List enumerates rows matching the supplied [ListFilter]. The
	// returned slice ordering is unspecified — callers that need
	// stable ordering must sort the result. Per-element defensive
	// copy of the `Participants` slice.
	//
	// The in-memory adapter requires a non-zero
	// `filter.OrganizationID` (it has no ambient session GUC to fall
	// back on); the Postgres adapter ignores the field and relies on
	// the RLS policy keyed off `watchkeeper.org`.
	List(ctx context.Context, filter ListFilter) ([]Conversation, error)

	// Close transitions the row matching `id` from [StatusOpen] to
	// [StatusArchived], stamps `ClosedAt`, and records `reason` as
	// the close rationale. The lifecycle transition is strictly
	// open→archived:
	//   - Unknown id — [ErrConversationNotFound].
	//   - Row already in [StatusArchived] — [ErrAlreadyArchived].
	//
	// `reason` may be empty (the M1.7 archive-on-summary writer
	// populates it; the M1.6 escalation auto-archive supplies a
	// stable sentinel).
	Close(ctx context.Context, id uuid.UUID, reason string) error

	// IncTokens monotonically advances the running token counter on
	// an open row. The supplied `delta` must be positive; non-positive
	// values surface [ErrInvalidTokenDelta] before any persistence
	// side effect.
	//
	// Resolution order:
	//   1. ctx.Err — refuses a pre-cancelled ctx.
	//   2. delta > 0 — [ErrInvalidTokenDelta] otherwise.
	//   3. row lookup — miss surfaces [ErrConversationNotFound].
	//   4. row.Status == [StatusOpen] — [ErrAlreadyArchived]
	//      otherwise (the M1.5 enforcement layer must not credit
	//      tokens against a closed row).
	//   5. atomic increment — the in-memory adapter holds the write
	//      lock for the full read-modify-write; the Postgres adapter
	//      uses `UPDATE ... SET tokens_used = tokens_used + $1
	//      WHERE id = $2 AND status = 'open' RETURNING tokens_used`
	//      so concurrent increments compose correctly under
	//      Postgres' row-level locking.
	//
	// Returns the post-increment `tokens_used` so the M1.5 enforcement
	// layer can compare against `token_budget` without a follow-up
	// [Get] round-trip.
	IncTokens(ctx context.Context, id uuid.UUID, delta int64) (int64, error)

	// BindSlackChannel stamps `slackChannelID` onto an existing open
	// row. Driven by the M1.1.c lifecycle wiring AFTER a successful
	// `CreateChannel` call and BEFORE the `InviteToChannel` fan-out:
	// the K2K Open() flow opens the repository row (which mints the
	// conversation id), uses that id to derive the Slack channel name,
	// calls Slack `conversations.create`, binds the returned channel id
	// back onto the row, then fans out the participant invites.
	// Bind-before-invite ordering (iter-1 codex Major fix) is
	// load-bearing: a concurrent `Close` racing the Open() between
	// `CreateChannel` and the bind would archive the row while leaving
	// the Slack channel live and unreachable (`Close` skips
	// `ArchiveChannel` when SlackChannelID is empty, by design for the
	// orphan-row path). Binding before the invite makes the row+channel
	// pair atomically consistent from the moment any reader sees the
	// row.
	//
	// Resolution order:
	//   1. ctx.Err — refuses a pre-cancelled ctx.
	//   2. `slackChannelID` non-empty after trim — [ErrEmptySlackChannelID].
	//   3. row lookup — miss surfaces [ErrConversationNotFound].
	//   4. row.Status == [StatusOpen] — [ErrAlreadyArchived]
	//      otherwise. Binding a Slack channel onto an archived row is a
	//      programmer bug (the lifecycle layer archives downstream of
	//      bind, never upstream).
	//   5. existing row.SlackChannelID empty — re-binding a row that
	//      already carries a channel id is a programmer bug
	//      ([ErrSlackChannelAlreadyBound]). The lifecycle layer is the
	//      sole writer and binds at most once per conversation;
	//      idempotent recovery on a duplicate-Open lives in the lifecycle
	//      layer's `CreateChannel` `name_taken` resolution, not here.
	//
	// On success the row reflects the supplied channel id and a
	// subsequent [Get] / [List] returns the bound value.
	BindSlackChannel(ctx context.Context, id uuid.UUID, slackChannelID string) error
}
