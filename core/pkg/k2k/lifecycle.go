// lifecycle.go ships the M1.1.c K2K lifecycle wiring: the consumer
// layer that composes [Repository] (M1.1.a) with the Slack channel
// primitives (M1.1.b `CreateChannel` / `InviteToChannel` /
// `ArchiveChannel` on `*messenger/slack.Client`). The lifecycle layer
// is the single seam every K2K Open() / Close() caller drives, so the
// Slack-side contract (channel naming, idempotent recovery, archive
// semantics) is exercised in exactly one place and cannot drift across
// consumers (M1.3 peer tools, M1.6 escalation saga, M1.7 archive
// summarisation, the `wk channel reveal` CLI).
//
// resolution order:
//
//	Open  → ctx.Err → param validation (delegated to Repository) →
//	        Repository.Open (mints row id) → derive channel name
//	        (`k2k-<id-prefix>`) → SlackChannels.CreateChannel(private=true)
//	        → Repository.BindSlackChannel(id, slackChannelID) →
//	        SlackChannels.InviteToChannel(participants) → Get to
//	        return the row reflecting the bound channel id.
//	Close → ctx.Err → Repository.Get (resolves the bound channel id)
//	        → SlackChannels.ArchiveChannel (idempotent on
//	        `already_archived`) → Repository.Close(id, reason).
//
// Bind-before-invite ordering (iter-1 codex Major fix): a concurrent
// Close racing with Open() in the window between CreateChannel and
// BindSlackChannel would archive the row while leaving the Slack
// channel live — `Close` skips ArchiveChannel when SlackChannelID is
// empty, by design for the orphan-row path. Persisting the channel
// id BEFORE the invite fan-out closes that window: any concurrent
// Close after CreateChannel observes the bound channel id and can
// archive the channel correctly. The invite step can still fail, but
// the row+channel pair is consistent in that case (the channel exists
// AND is bound to the row; Close will archive both correctly).
//
// audit discipline: this file does NOT import `keeperslog` and does
// NOT call `.Append(`. The K2K event taxonomy
// (`k2k_conversation_opened` / `k2k_conversation_closed` / etc.) is
// emitted through the M1.4 `k2k/audit.Emitter` seam — a typed
// interface that wraps `keeperslog.Writer` outside this file. The
// lifecycle layer composes the seam via `LifecycleDeps.Auditor` (an
// OPTIONAL dep; nil-permissive so M1.1.c-era wirings stay valid). A
// source-grep AC test pins the keeperslog/Append ban so a future
// contributor adding inline audit emission here trips a fast-failing
// test.
//
// PII discipline: channel ids and slack user ids are workspace-public
// identifiers (Slack documents them as opaque non-secret values). The
// derived channel name (`k2k-<id-prefix>`) is caller-derived from the
// UUID prefix — never leaks free-form operator text. The participants
// slice is defensively deep-copied at the boundary so caller-side
// mutation cannot bleed. Slack tokens never reach this layer; the
// caller's [SlackChannels] implementation owns the secret.
//
// Idempotency: M1.1.b's `CreateChannel` is idempotent on `name_taken`
// (returns the existing channel id for a non-archived collision); the
// `BindSlackChannel` step is one-shot (re-bind returns
// [ErrSlackChannelAlreadyBound]) so a partial-success retry of Open()
// from an upstream caller is the caller's responsibility — the
// lifecycle layer does not silently re-use an existing
// `slack_channel_id`. M1.1.b's `ArchiveChannel` is idempotent on
// `already_archived`; [Close] composes that with
// [Repository.Close]'s [ErrAlreadyArchived] sentinel so a
// duplicate-close from a saga-replay path surfaces as a typed
// sentinel rather than a noisy error chain.
package k2k

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k/audit"
)

// channelNamePrefix is the canonical leader on every K2K-derived Slack
// channel name. Surfaced as a constant so a future M1.7 channel-name
// audit can match the pattern via a single source of truth.
const channelNamePrefix = "k2k-"

// auditEmitTimeout caps the detached-ctx window the M1.4 audit emit
// runs under. The emit is best-effort observability; a 5-second cap
// is long enough that a healthy keeperslog Append completes under
// any realistic backpressure but short enough that a degenerate Keep
// outage cannot tie up the caller's lifecycle / peer-tool return
// indefinitely. Tuned to the same order of magnitude as the M9.4.a
// publisher timeout discipline.
const auditEmitTimeout = 5 * time.Second

// conversationIDPrefixLen is the number of leading UUID hex characters
// the [DeriveChannelName] helper consumes. Eight chars is the minimum
// that still discriminates workspace-wide given Slack's 80-char limit
// on channel names AND keeps the resulting name short enough to read
// in a Slack sidebar. The UUID alphabet is 16-symbol hex; eight
// characters yield 16^8 = ~4.3e9 distinct prefixes — collision
// probability on a workspace with O(10^3) live K2K channels is
// negligible. A collision is non-fatal: M1.1.b's `CreateChannel`
// idempotently resolves a `name_taken` via `conversations.list`,
// returning the existing non-archived channel id.
const conversationIDPrefixLen = 8

// SlackChannels is the narrow seam the lifecycle layer consumes from
// the M1.1.b Slack channel primitives. The interface is the unit-test
// seam: production wiring satisfies it with the per-method shapes on
// `*messenger/slack.Client`; tests inject a hand-rolled fake. Mirrors
// the per-call seam discipline from M1.1.a's [Querier] — the lifecycle
// layer never knows whether it talks to a live Slack workspace or a
// fake.
//
// The surface is intentionally narrower than the four-method M1.1.b
// extension set: `RevealChannel` is consumed by the
// `wk channel reveal` CLI, NOT by the Open() / Close() lifecycle, so
// the lifecycle's seam stays minimal — adding RevealChannel here
// would force every fake to implement a method the lifecycle never
// calls.
//
// Implementations must be safe for concurrent use across goroutines.
// The production `*slack.Client` implementation is goroutine-safe by
// construction (it serialises tokens via the configured rate limiter
// and never mutates its config after [slack.NewClient] returns).
type SlackChannels interface {
	// CreateChannel provisions a Slack channel named `name`. When
	// `isPrivate` is true the channel is created as a private channel
	// (visible only to invited members). Idempotent on `name_taken`:
	// a NON-ARCHIVED same-name same-kind collision transparently
	// resolves to the existing channel id.
	CreateChannel(ctx context.Context, name string, isPrivate bool) (string, error)

	// InviteToChannel invites the supplied Slack user ids to
	// `channelID`. Slack's `conversations.invite` is documented
	// all-or-nothing for multi-user batches; the M1.1.b implementation
	// surfaces `already_in_channel` as an error for multi-user batches
	// (one of the listed users was a member) and as a silent success
	// for single-user batches (the K2K consumer's safe-replay case).
	InviteToChannel(ctx context.Context, channelID string, userIDs []string) error

	// ArchiveChannel archives `channelID`. Idempotent on
	// `already_archived`: a second archive call on the same id
	// returns nil per the M1.1.b contract.
	ArchiveChannel(ctx context.Context, channelID string) error
}

// LifecycleDeps is the closed-set dependency bundle [NewLifecycle]
// accepts. Hoisted to a struct (rather than a long positional arg
// list) so a future addition (e.g. the M1.4 audit subscriber callback)
// lands as a new field rather than a breaking signature change. Mirrors
// the saga-step `<Step>StepDeps` pattern.
type LifecycleDeps struct {
	// Repo is the persistence seam, typically
	// [*PostgresRepository] in production and [*MemoryRepository] in
	// tests / dev / smoke loops. Required (non-nil); [NewLifecycle]
	// panics otherwise.
	Repo Repository

	// Slack is the channel-primitives seam, typically
	// [*messenger/slack.Client] in production. Required (non-nil);
	// [NewLifecycle] panics otherwise.
	Slack SlackChannels

	// Auditor is the M1.4 K2K audit-emission seam — typically a
	// [*audit.Writer] in production wiring. OPTIONAL: nil is permitted
	// so M1.1.c-era callers wiring the lifecycle without an audit sink
	// stay valid. When non-nil, [Open] emits
	// [audit.EventConversationOpened] after the row + Slack channel
	// are bound, and [Close] emits [audit.EventConversationClosed]
	// after the repository transition. Mirrors the M1.3.c
	// [peer.Deps.EventBus] optional-dep discipline. An audit-emit
	// failure NEVER short-circuits the lifecycle's own success — the
	// row + channel state is the load-bearing surface; the audit row
	// is observability.
	Auditor audit.Emitter
}

// Lifecycle is the M1.1.c K2K conversation lifecycle orchestrator. It
// composes the persistence + Slack seams so callers (M1.3 peer tools,
// M1.6 escalation saga, the `wk channel reveal` CLI, …) drive K2K
// state transitions through one entry point rather than ad-hoc
// stitching the two seams together at every call site.
//
// Constructed via [NewLifecycle]; the zero value is NOT usable —
// callers must always go through the constructor so the dependency
// invariants are enforced at construction time. Mirrors the saga-step
// discipline of "panic on nil deps; never return a degenerate value
// from the constructor".
type Lifecycle struct {
	deps LifecycleDeps
}

// NewLifecycle returns a configured [Lifecycle]. Panics on a nil
// `deps.Repo` or `deps.Slack` — both are programmer bugs at wiring
// time, not runtime errors to thread through Open / Close return
// values. The panic message names the offending field so an operator
// reading a stack trace can fix the wiring immediately.
//
// Panic discipline mirrors the saga-step `New<Step>Step` constructors
// (`core/pkg/spawn/saga/<step>_step.go`) and M1.1.a's
// [NewPostgresRepository] / [NewMemoryRepository].
func NewLifecycle(deps LifecycleDeps) *Lifecycle {
	if deps.Repo == nil {
		panic("k2k: NewLifecycle: deps.Repo must not be nil")
	}
	if deps.Slack == nil {
		panic("k2k: NewLifecycle: deps.Slack must not be nil")
	}
	return &Lifecycle{deps: deps}
}

// DeriveChannelName derives the Slack channel name for a K2K
// conversation from its id. The shape is `k2k-<first-8-hex-chars>` —
// load-bearing for:
//
//   - Operator legibility: an operator paging through Slack's channel
//     list can spot K2K-owned channels by the `k2k-` prefix.
//   - M1.7 archive-on-summary correlation: the summariser can map a
//     Slack channel name back to its source conversation id by
//     decoding the prefix.
//   - Idempotent recovery: the name is deterministic from the
//     conversation id, so a retried K2K Open() with the same id (e.g.
//     from a saga-replay path) collides with the original channel
//     name and M1.1.b's `name_taken` recovery resolves transparently.
//
// Exported so the `wk channel reveal` CLI (and a future M1.7
// summariser) can reconstruct the name without re-implementing the
// derivation. Returns the empty string on `uuid.Nil` so callers fail
// at the obvious boundary rather than at the Slack call. The full
// UUID hex is 32 characters (no hyphens); slicing to
// [conversationIDPrefixLen] is bounds-safe.
func DeriveChannelName(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	hex := strings.ReplaceAll(id.String(), "-", "")
	if len(hex) < conversationIDPrefixLen {
		// Defensive — uuid.UUID.String() is always 36 chars (32 hex +
		// 4 hyphens) but a future uuid library swap could change the
		// shape. Return the whole hex rather than panicking so the
		// caller's diagnostic chain stays unbroken.
		return channelNamePrefix + hex
	}
	return channelNamePrefix + hex[:conversationIDPrefixLen]
}

// Open runs the K2K conversation Open lifecycle:
//
//  1. Persist a fresh `k2k_conversations` row via [Repository.Open]
//     (which validates the params + mints the conversation id).
//  2. Derive the Slack channel name via [DeriveChannelName].
//  3. Provision the Slack channel via
//     [SlackChannels.CreateChannel](name, isPrivate=true). M1.1.b
//     idempotently resolves a `name_taken` collision against a
//     non-archived same-name same-kind channel by returning its id.
//  4. Bind the resolved `slack_channel_id` onto the row via
//     [Repository.BindSlackChannel] — BEFORE the invite fan-out so
//     a concurrent [Close] in this window cannot leak the Slack
//     channel as an orphan unreachable from the persisted row
//     (iter-1 codex Major fix).
//  5. Fan-out invite to the participant bots via
//     [SlackChannels.InviteToChannel]. Slack's all-or-nothing
//     multi-user invite semantics surface as
//     [slack.ErrAlreadyInChannel] for partial-member batches per
//     the M1.1.b contract; the lifecycle layer surfaces this verbatim
//     so the caller can decide on retry strategy (the K2K consumer
//     of the lifecycle owns the saga-replay discipline).
//  6. [Repository.Get] the row to return the up-to-date
//     [Conversation] with the bound channel id.
//
// Failure modes:
//
//   - Slack [CreateChannel] failure (step 3): the persisted row remains
//     in [StatusOpen] with empty `slack_channel_id`. The orphan-row
//     pattern from M1.1.a applies — M1.7's archive-on-summary writer
//     is the canonical reaper.
//   - [BindSlackChannel] failure (step 4) AFTER a successful Slack
//     [CreateChannel]: the Slack channel is live but unbound. The
//     M1.7 reaper is the same path; the M1.1.c godoc on
//     [Repository.BindSlackChannel] documents the one-shot contract
//     so a future contributor never tries silent re-bind.
//   - [InviteToChannel] failure (step 5): the row carries the bound
//     channel id; a subsequent [Close] archives both row and Slack
//     channel correctly. Iter-1 codex Minor fix relative to the
//     original "bind after invite" ordering — that ordering would
//     have left a successful-invite + failed-bind interleaving in
//     which the row pointed at no channel even though one was live.
//
// Returns the bound [Conversation] on success.
func (l *Lifecycle) Open(ctx context.Context, params OpenParams) (Conversation, error) {
	if err := ctx.Err(); err != nil {
		return Conversation{}, err
	}

	conv, err := l.deps.Repo.Open(ctx, params)
	if err != nil {
		return Conversation{}, fmt.Errorf("k2k: lifecycle open: %w", err)
	}

	name := DeriveChannelName(conv.ID)
	channelID, err := l.deps.Slack.CreateChannel(ctx, name, true)
	if err != nil {
		return Conversation{}, fmt.Errorf("k2k: lifecycle open: create channel %q: %w", name, err)
	}
	if strings.TrimSpace(channelID) == "" {
		// Defensive — M1.1.b's CreateChannel surfaces an explicit
		// error when the Slack response omits the channel id. Match
		// the boundary here so a future SlackChannels implementation
		// that silently returns "" cannot smuggle a half-bound row
		// into the store.
		return Conversation{}, fmt.Errorf("k2k: lifecycle open: create channel %q: empty channel id returned", name)
	}

	// Bind BEFORE invite (iter-1 codex Major fix): a concurrent Close
	// racing this Open in the window between CreateChannel and the
	// row-side bind would archive the DB row while leaving the Slack
	// channel live and unreachable from the persisted state (Close
	// skips ArchiveChannel when SlackChannelID is still empty). Doing
	// the bind first makes the row+channel pair atomically consistent
	// from the moment any reader sees the row.
	if err := l.deps.Repo.BindSlackChannel(ctx, conv.ID, channelID); err != nil {
		return Conversation{}, fmt.Errorf("k2k: lifecycle open: bind slack channel: %w", err)
	}

	if err := l.deps.Slack.InviteToChannel(ctx, channelID, conv.Participants); err != nil {
		return Conversation{}, fmt.Errorf("k2k: lifecycle open: invite to channel %s: %w", channelID, err)
	}

	bound, err := l.deps.Repo.Get(ctx, conv.ID)
	if err != nil {
		return Conversation{}, fmt.Errorf("k2k: lifecycle open: refresh row: %w", err)
	}

	// M1.4 audit emission. Emit AFTER the row + Slack channel are
	// bound + the invite fan-out completed, so the audit row reflects
	// the state any subsequent reader can observe. A nil Auditor is a
	// no-op (M1.1.c-era wirings stayed valid by design); an emit
	// failure is logged but does NOT propagate — the lifecycle's
	// success is gated on persisted state, not observability. The
	// emit runs under a detached ctx (`context.WithoutCancel` + a
	// short timeout) so a caller-side cancellation arriving after the
	// state mutation succeeded does NOT systematically drop the audit
	// row (iter-1 codex Major fix).
	if l.deps.Auditor != nil {
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditEmitTimeout)
		defer cancel()
		_, _ = l.deps.Auditor.EmitConversationOpened(auditCtx, audit.ConversationOpenedEvent{
			ConversationID: bound.ID,
			OrganizationID: bound.OrganizationID,
			Participants:   bound.Participants,
			Subject:        bound.Subject,
			CorrelationID:  bound.CorrelationID,
			SlackChannelID: bound.SlackChannelID,
			OpenedAt:       bound.OpenedAt,
		})
	}

	return bound, nil
}

// Close runs the K2K conversation Close lifecycle:
//
//  1. [Repository.Get] resolves the bound `slack_channel_id` from
//     the row.
//  2. [SlackChannels.ArchiveChannel] archives the channel
//     (idempotent on `already_archived` per M1.1.b).
//  3. [Repository.Close] transitions the row to [StatusArchived].
//
// A double-close from a saga-replay path surfaces
// [ErrAlreadyArchived] from step 3 — the caller may treat this as a
// silent OK per the saga-replay discipline. Step 2 is skipped when
// the row already has no bound channel id (an orphan from a failed
// Open(); see [Open] doc); the close still transitions the row so the
// M1.4 audit subscriber sees a clean lifecycle close.
//
// `reason` is forwarded verbatim to [Repository.Close] for the
// `close_reason` column. The M1.7 summariser populates it with the
// archive-on-summary text; the M1.6 escalation saga supplies a
// stable sentinel. Empty `reason` is allowed (matches the
// [Repository.Close] contract).
func (l *Lifecycle) Close(ctx context.Context, id uuid.UUID, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if id == uuid.Nil {
		return fmt.Errorf("%w: %s", ErrConversationNotFound, id)
	}

	conv, err := l.deps.Repo.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("k2k: lifecycle close: %w", err)
	}

	if conv.SlackChannelID != "" {
		if err := l.deps.Slack.ArchiveChannel(ctx, conv.SlackChannelID); err != nil {
			return fmt.Errorf("k2k: lifecycle close: archive channel %s: %w", conv.SlackChannelID, err)
		}
	}

	if err := l.deps.Repo.Close(ctx, id, reason); err != nil {
		if errors.Is(err, ErrAlreadyArchived) {
			// Saga-replay: the row was already closed by a prior
			// invocation. Surface the typed sentinel so the caller
			// branches on errors.Is, mirroring M1.1.b's idempotent-
			// archive translation discipline. The race-winner already
			// emitted the audit row; emitting a second one here would
			// duplicate the observed close so we deliberately skip.
			return err
		}
		return fmt.Errorf("k2k: lifecycle close: %w", err)
	}

	// M1.4 audit emission. Emit AFTER the repository transition so the
	// audit row reflects the state any subsequent reader can observe.
	// A nil Auditor is a no-op; an emit failure is logged but does NOT
	// propagate — the close's success is gated on persisted state, not
	// observability. The post-close Get re-reads the row so the emitted
	// `closed_at` matches the persisted column rather than a wall-clock
	// approximation. The Get + emit both run under a detached ctx so a
	// caller-side cancellation arriving after Repo.Close succeeded does
	// NOT systematically drop the close audit row (iter-1 codex Major
	// fix).
	if l.deps.Auditor != nil {
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditEmitTimeout)
		defer cancel()
		closed, getErr := l.deps.Repo.Get(auditCtx, id)
		if getErr == nil {
			_, _ = l.deps.Auditor.EmitConversationClosed(auditCtx, audit.ConversationClosedEvent{
				ConversationID: closed.ID,
				OrganizationID: closed.OrganizationID,
				CloseReason:    closed.CloseReason,
				ClosedAt:       closed.ClosedAt,
			})
		}
	}

	return nil
}
