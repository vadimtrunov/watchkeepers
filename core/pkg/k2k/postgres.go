// postgres.go ships the Postgres-backed [Repository] implementation
// the M1.1.c lifecycle wiring will plug in production. The impl is
// intentionally thin: every method maps 1:1 to a single SQL statement
// against the `watchkeeper.k2k_conversations` table created by
// `deploy/migrations/029_k2k_conversations.sql`.
//
// RLS discipline: this adapter holds a [Querier] (any
// pgx-compatible querier — typically a [pgx.Tx] obtained from
// `core/internal/keep/db.WithScope`), NOT a [pgxpool.Pool]. The
// production wiring is:
//
//	pool.BeginTx → SET LOCAL ROLE wk_*_role → SET LOCAL watchkeeper.org
//	  → k2k.NewPostgresRepository(tx, nil) → repo.Open / Get / List / ...
//
// Wiring the adapter over the tx (not the pool) is load-bearing: a
// [pgxpool.Pool] checks out an arbitrary backend connection per
// statement, so a `SET LOCAL` issued on connection A is invisible to
// the next statement that lands on connection B. Without the per-tx
// scoping every Open / Get / List would either see zero rows (RLS
// USING NULL) or be rejected at WITH CHECK (INSERT under unset GUC).
// The [Querier] alias preserves the "take a per-call seam" discipline
// from `core/internal/keep/db.WithScope` while letting the adapter
// re-use one struct shape across reads + writes.
//
// An unset GUC is fail-closed at the Postgres layer (zero rows
// visible, no INSERT permitted) per the migration's
// `nullif(..., ”)::uuid` cast, so a misconfigured caller surfaces as
// `ErrConversationNotFound` (read) or an RLS policy violation
// (write) rather than silent cross-tenant access.
//
// Concurrency: every method is safe for concurrent use across
// goroutines on the same [PostgresRepository] value PROVIDED the
// underlying [Querier] is concurrency-safe. A [pgx.Tx] is NOT (one
// goroutine per tx); a [pgxpool.Pool] is. Production wiring is
// per-request (one tx per inbound HTTP request via `WithScope`), so
// the per-tx-not-safe constraint is satisfied implicitly. Background
// workers that hold a long-lived adapter should construct a fresh
// repository per goroutine.
//
// IncTokens race handling: the `UPDATE ... WHERE status = 'open'
// RETURNING tokens_used` shape relies on Postgres row-level locking
// — a concurrent close-then-increment surfaces zero rows and the
// adapter returns [ErrAlreadyArchived] after a follow-up status probe.
//
// This file ships at M1.1.a behind a compile-time assertion only; the
// concrete behaviour is exercised end-to-end by the M1.1.c lifecycle
// wiring + the `scripts/migrate-schema-test.sh` RLS assertions. The
// pattern mirrors the M6.3.b "ship in-memory DAO + tests with
// consumer; Postgres adapter is a follow-up paired with consumer
// wiring" lesson: shipping the adapter now keeps the M1.1.a AC
// satisfied without forcing test-container scaffolding into a leaf
// package whose consumers all live downstream.
package k2k

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the minimum pgx surface the Postgres adapter needs. Both
// [github.com/jackc/pgx/v5.Tx] and
// [github.com/jackc/pgx/v5/pgxpool.Pool] satisfy it; production
// wiring passes the [pgx.Tx] from
// [core/internal/keep/db.WithScope] so the adapter's statements run
// under the caller's `SET LOCAL ROLE` + `SET LOCAL watchkeeper.org`
// session state. Test wiring may pass a mock satisfying the same
// surface.
//
// Mirrors the "narrow interface at the integration seam" discipline
// from `keep/server/handlers_read.scopedRunner` (the handler-side
// counterpart that wraps the same scoping contract).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// PostgresRepository is the [Repository] implementation backed by the
// `watchkeeper.k2k_conversations` table. Constructed via
// [NewPostgresRepository]; the zero value is NOT usable — callers
// must always go through the constructor so the underlying querier
// reference is non-nil.
//
// `pollInterval` controls the [WaitForReply] polling cadence. The M1.3.a
// AC pins "Postgres uses LISTEN/NOTIFY on a per-conversation channel,
// with a polling fallback" — this adapter ships the polling fallback
// only; a future iteration may layer LISTEN/NOTIFY behind the same
// [Repository.WaitForReply] signature without a wire-shape break.
type PostgresRepository struct {
	q            Querier
	now          func() time.Time
	pollInterval time.Duration
}

// defaultWaitForReplyPollInterval is the polling cadence used by
// [PostgresRepository.WaitForReply] when [NewPostgresRepository]'s
// `pollInterval` argument is zero. 100ms balances responsiveness with
// query load — a request-reply round-trip on the M1.3.a peer.* family
// has a human-perceptible latency budget of 1-5s in practice, so a
// 100ms poll loop adds at most one tick of jitter.
const defaultWaitForReplyPollInterval = 100 * time.Millisecond

// Compile-time assertion: [*PostgresRepository] satisfies
// [Repository]. Pins the integration shape so a future change to the
// interface surface fails the build here.
var _ Repository = (*PostgresRepository)(nil)

// NewPostgresRepository returns a Postgres-backed [Repository] over
// the supplied [Querier] (typically the [pgx.Tx] from
// [core/internal/keep/db.WithScope]). Panics on a nil querier — a
// nil querier is a programmer bug at wiring time, not a runtime error
// to thread through error returns (mirrors the saga step
// constructors' panic-on-nil-deps discipline). The optional `now`
// argument overrides the wall-clock used to stamp `OpenedAt` /
// `ClosedAt`; pass nil to use [time.Now].
func NewPostgresRepository(q Querier, now func() time.Time) *PostgresRepository {
	if q == nil {
		panic("k2k: NewPostgresRepository: querier must not be nil")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresRepository{q: q, now: now, pollInterval: defaultWaitForReplyPollInterval}
}

// WithPollInterval overrides the [WaitForReply] polling cadence on the
// adapter. Test wiring uses a short interval (e.g. 1ms) so the
// integration tests do not pay the default 100ms tick; production
// wiring leaves the default. Non-positive `d` is a no-op.
func (r *PostgresRepository) WithPollInterval(d time.Duration) *PostgresRepository {
	if d > 0 {
		r.pollInterval = d
	}
	return r
}

// Open implements [Repository.Open]. Validation order matches the
// in-memory adapter (ctx → org → subject → participants → budget) so
// the two impls fail-fast on the same inputs with the same sentinels.
// The INSERT uses `RETURNING opened_at` so the caller observes the
// server-stamped `OpenedAt` (which may differ from `r.now()` if the
// SQL DEFAULT fires; we still pass `r.now()` for the explicit column
// so a fixture clock in a future integration test can pin the value).
//
// `correlation_id` is a typed [uuid.UUID] on the Go side and a `uuid
// NULL` column on the SQL side. A zero-valued [uuid.Nil] from the
// caller maps to SQL NULL via a nilable `*uuid.UUID` parameter; a
// non-zero UUID flows verbatim. This contract is symmetric with the
// in-memory adapter, which stores the zero / non-zero distinction
// directly in the struct.
func (r *PostgresRepository) Open(ctx context.Context, params OpenParams) (Conversation, error) {
	if err := ctx.Err(); err != nil {
		return Conversation{}, err
	}
	if params.OrganizationID == uuid.Nil {
		return Conversation{}, ErrEmptyOrganization
	}
	if strings.TrimSpace(params.Subject) == "" {
		return Conversation{}, ErrEmptySubject
	}
	if len(params.Participants) == 0 {
		return Conversation{}, ErrEmptyParticipants
	}
	for _, p := range params.Participants {
		if strings.TrimSpace(p) == "" {
			return Conversation{}, ErrEmptyParticipants
		}
	}
	if params.TokenBudget < 0 {
		return Conversation{}, ErrInvalidTokenBudget
	}

	// Defensive deep-copy before handing to pgx so a caller-side
	// mutation between Open returning and pgx serializing the param
	// cannot bleed.
	participants := make([]string, len(params.Participants))
	copy(participants, params.Participants)

	id := uuid.New()
	now := r.now().UTC()

	// `*uuid.UUID` so pgx serialises uuid.Nil as SQL NULL (a nil
	// pointer) and a non-zero UUID as the underlying value. Mirrors
	// pgx's standard "nilable type ↔ SQL NULL" idiom and avoids the
	// `NULLIF($n, '')::uuid` trick which would only work if the wire
	// value were text.
	var correlationParam *uuid.UUID
	if params.CorrelationID != uuid.Nil {
		c := params.CorrelationID
		correlationParam = &c
	}

	const insertSQL = `
INSERT INTO watchkeeper.k2k_conversations (
  id, organization_id, slack_channel_id, participants, subject,
  status, token_budget, tokens_used, opened_at, correlation_id, close_reason
) VALUES (
  $1, $2, NULL, $3, $4, 'open', $5, 0, $6, $7, ''
)
RETURNING opened_at`

	var openedAt time.Time
	if err := r.q.QueryRow(
		ctx, insertSQL,
		id, params.OrganizationID, participants, params.Subject,
		params.TokenBudget, now, correlationParam,
	).Scan(&openedAt); err != nil {
		return Conversation{}, fmt.Errorf("k2k: open: %w", err)
	}

	return Conversation{
		ID:             id,
		OrganizationID: params.OrganizationID,
		Participants:   participants,
		Subject:        params.Subject,
		Status:         StatusOpen,
		TokenBudget:    params.TokenBudget,
		TokensUsed:     0,
		OpenedAt:       openedAt,
		CorrelationID:  params.CorrelationID,
	}, nil
}

// scanConversation centralises the row→Conversation projection used
// by both [Get] and [List]. Keeps the nullable-column handling
// (`closed_at`, `correlation_id`, `slack_channel_id`) in one place so
// a future schema change touches a single scan target.
func scanConversation(row pgx.Row) (Conversation, error) {
	var (
		c                Conversation
		slackChannelID   *string
		statusText       string
		closedAt         *time.Time
		correlationID    *uuid.UUID
		closeReasonText  *string
		fetchedTokenBdgt int64
		fetchedTokensUsd int64
	)

	if err := row.Scan(
		&c.ID, &c.OrganizationID, &slackChannelID,
		&c.Participants, &c.Subject, &statusText,
		&fetchedTokenBdgt, &fetchedTokensUsd,
		&c.OpenedAt, &closedAt,
		&correlationID, &closeReasonText,
	); err != nil {
		return Conversation{}, err
	}

	c.Status = Status(statusText)
	c.TokenBudget = fetchedTokenBdgt
	c.TokensUsed = fetchedTokensUsd
	if slackChannelID != nil {
		c.SlackChannelID = *slackChannelID
	}
	if closedAt != nil {
		c.ClosedAt = *closedAt
	}
	if correlationID != nil {
		c.CorrelationID = *correlationID
	}
	if closeReasonText != nil {
		c.CloseReason = *closeReasonText
	}
	return c, nil
}

// selectColumns is the canonical column ordering both [Get] and
// [List] consume. Hoisted to a constant so a future schema delta
// touches one place.
const selectColumns = `id, organization_id, slack_channel_id,
       participants, subject, status, token_budget, tokens_used,
       opened_at, closed_at, correlation_id, close_reason`

// Get implements [Repository.Get]. A miss surfaces
// [ErrConversationNotFound] wrapped with the requested id; an RLS
// invisibility (caller is scoped to a different tenant) reaches the
// same branch — by design, since the cross-tenant row is not visible
// to the caller and the row "does not exist" from the caller's
// perspective.
func (r *PostgresRepository) Get(ctx context.Context, id uuid.UUID) (Conversation, error) {
	if err := ctx.Err(); err != nil {
		return Conversation{}, err
	}

	selectSQL := "SELECT " + selectColumns + ` FROM watchkeeper.k2k_conversations WHERE id = $1`

	c, err := scanConversation(r.q.QueryRow(ctx, selectSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, fmt.Errorf("%w: %s", ErrConversationNotFound, id)
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("k2k: get: %w", err)
	}
	return c, nil
}

// List implements [Repository.List]. Per the interface godoc, the
// Postgres adapter ignores `filter.OrganizationID` and relies on the
// RLS policy keyed off `watchkeeper.org`. `filter.Status` is applied
// as a WHERE-clause predicate when non-empty.
func (r *PostgresRepository) List(ctx context.Context, filter ListFilter) ([]Conversation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if filter.Status != "" {
		if err := filter.Status.Validate(); err != nil {
			return nil, err
		}
	}

	// `status = $1 OR $1 = ''` lets us share one prepared statement
	// across both filtered and unfiltered enumerations. Postgres
	// optimises out the OR-branch when the parameter is known at plan
	// time; at M1.1.a row counts the planner cost is irrelevant.
	listSQL := "SELECT " + selectColumns + ` FROM watchkeeper.k2k_conversations WHERE ($1 = '' OR status = $1)`

	rows, err := r.q.Query(ctx, listSQL, string(filter.Status))
	if err != nil {
		return nil, fmt.Errorf("k2k: list: %w", err)
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		c, err := scanConversation(rows)
		if err != nil {
			return nil, fmt.Errorf("k2k: list scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("k2k: list iterate: %w", err)
	}
	return out, nil
}

// Close implements [Repository.Close]. Uses a conditional UPDATE so
// the open→archived transition is atomic at the row level: an attempt
// to close an already-archived row matches zero rows and we surface
// [ErrAlreadyArchived] (after distinguishing it from "unknown id" via
// a follow-up SELECT). The two-statement shape is intentional — a
// single UPDATE would conflate "row missing" with "row already
// archived" on a zero rowcount.
func (r *PostgresRepository) Close(ctx context.Context, id uuid.UUID, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	now := r.now().UTC()

	const updateSQL = `
UPDATE watchkeeper.k2k_conversations
SET status = 'archived', closed_at = $1, close_reason = $2
WHERE id = $3 AND status = 'open'`

	tag, err := r.q.Exec(ctx, updateSQL, now, reason, id)
	if err != nil {
		return fmt.Errorf("k2k: close: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// Zero rows affected — either the id is unknown OR the row is
	// already archived. Disambiguate with a follow-up read so the
	// caller observes the correct sentinel.
	const probeSQL = `SELECT status FROM watchkeeper.k2k_conversations WHERE id = $1`
	var status string
	if err := r.q.QueryRow(ctx, probeSQL, id).Scan(&status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrConversationNotFound, id)
		}
		return fmt.Errorf("k2k: close probe: %w", err)
	}
	if status == string(StatusArchived) {
		return fmt.Errorf("%w: %s", ErrAlreadyArchived, id)
	}
	// Row exists but is in an unexpected state — surface as a wrapped
	// error so a future status addition that bypasses Close surfaces
	// loudly.
	return fmt.Errorf("k2k: close: unexpected status %q for id %s", status, id)
}

// BindSlackChannel implements [Repository.BindSlackChannel]. The
// conditional UPDATE matches only rows whose `slack_channel_id` is
// currently NULL AND whose status is 'open', so the bind is atomic at
// the row level: a duplicate Bind racing with the first one either
// matches zero rows (and the follow-up probe distinguishes
// already-bound from already-archived from unknown id) or wins the
// transition itself.
//
// The validator chain (ctx → trimmed id non-empty) runs BEFORE the
// SQL round-trip so a malformed input fails fast without burning a
// Postgres call.
func (r *PostgresRepository) BindSlackChannel(ctx context.Context, id uuid.UUID, slackChannelID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(slackChannelID)
	if trimmed == "" {
		return ErrEmptySlackChannelID
	}

	const updateSQL = `
UPDATE watchkeeper.k2k_conversations
SET slack_channel_id = $1
WHERE id = $2 AND status = 'open' AND slack_channel_id IS NULL`

	tag, err := r.q.Exec(ctx, updateSQL, trimmed, id)
	if err != nil {
		return fmt.Errorf("k2k: bind slack channel: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// Zero rows affected — disambiguate via a follow-up SELECT to
	// surface the precise sentinel. Mirrors [Close]'s discipline so the
	// caller can distinguish "unknown id" / "already archived" /
	// "already bound" without parsing the error chain.
	const probeSQL = `SELECT status, slack_channel_id FROM watchkeeper.k2k_conversations WHERE id = $1`
	var (
		status         string
		existingChanID *string
	)
	if probeErr := r.q.QueryRow(ctx, probeSQL, id).Scan(&status, &existingChanID); probeErr != nil {
		if errors.Is(probeErr, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrConversationNotFound, id)
		}
		return fmt.Errorf("k2k: bind slack channel probe: %w", probeErr)
	}
	if status == string(StatusArchived) {
		return fmt.Errorf("%w: %s", ErrAlreadyArchived, id)
	}
	if existingChanID != nil && *existingChanID != "" {
		return fmt.Errorf("%w: %s", ErrSlackChannelAlreadyBound, id)
	}
	// Row exists in 'open' state with NULL channel id but the UPDATE
	// still matched zero rows. Should not happen under normal
	// operation; surface as a wrapped error so a future state-machine
	// addition surfaces loudly.
	return fmt.Errorf("k2k: bind slack channel: unexpected zero rows for id %s (status=%q)", id, status)
}

// IncTokens implements [Repository.IncTokens]. The atomic increment
// via `UPDATE ... SET tokens_used = tokens_used + $1 ...
// RETURNING tokens_used` composes correctly under concurrent callers
// — Postgres row-level locking serialises the read-modify-write
// inside the executor.
func (r *PostgresRepository) IncTokens(ctx context.Context, id uuid.UUID, delta int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if delta <= 0 {
		return 0, fmt.Errorf("%w: %d", ErrInvalidTokenDelta, delta)
	}

	const updateSQL = `
UPDATE watchkeeper.k2k_conversations
SET tokens_used = tokens_used + $1
WHERE id = $2 AND status = 'open'
RETURNING tokens_used`

	var tokensUsed int64
	err := r.q.QueryRow(ctx, updateSQL, delta, id).Scan(&tokensUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		// Zero rows — either unknown id or row not open. Disambiguate
		// via a follow-up SELECT, matching [Close]'s discipline.
		const probeSQL = `SELECT status FROM watchkeeper.k2k_conversations WHERE id = $1`
		var status string
		if probeErr := r.q.QueryRow(ctx, probeSQL, id).Scan(&status); probeErr != nil {
			if errors.Is(probeErr, pgx.ErrNoRows) {
				return 0, fmt.Errorf("%w: %s", ErrConversationNotFound, id)
			}
			return 0, fmt.Errorf("k2k: inc tokens probe: %w", probeErr)
		}
		return 0, fmt.Errorf("%w: %s", ErrAlreadyArchived, id)
	}
	if err != nil {
		return 0, fmt.Errorf("k2k: inc tokens: %w", err)
	}
	return tokensUsed, nil
}

// AppendMessage implements [Repository.AppendMessage]. Validation order
// matches the in-memory adapter so the two impls fail-fast on the same
// inputs with the same sentinels. The INSERT uses `RETURNING created_at`
// so the caller observes the server-stamped timestamp (which may differ
// from `r.now()` if the SQL DEFAULT fires).
//
// The append guards on conversation status via a single
// `INSERT ... SELECT FROM k2k_conversations WHERE status='open'`
// subquery so a concurrent `Lifecycle.Close` between the caller's
// `Get` and the append can NOT slip a write into an archived row.
// When the subquery returns zero rows, the INSERT inserts zero rows
// and the caller observes [ErrConversationNotOpen] (distinguishing
// "open conversation that lost the race" from "FK violation /
// unknown id"). FK violations on `conversation_id` still surface as
// pgx 23503.
//
// `body` is bound as raw `[]byte`; pgx encodes the parameter as
// `bytea` against the binary-safe column from migration 030. The
// peer-tool API documents `Body` as opaque bytes — a future M1.3.c
// subscription delivery may legitimately carry a serialised
// protobuf with non-UTF-8 bytes that a `text` column would corrupt.
func (r *PostgresRepository) AppendMessage(ctx context.Context, params AppendMessageParams) (Message, error) {
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}
	if params.ConversationID == uuid.Nil {
		return Message{}, ErrEmptyConversationID
	}
	if params.OrganizationID == uuid.Nil {
		return Message{}, ErrEmptyOrganization
	}
	if strings.TrimSpace(params.SenderWatchkeeperID) == "" {
		return Message{}, ErrEmptySenderWatchkeeperID
	}
	if len(params.Body) == 0 {
		return Message{}, ErrEmptyMessageBody
	}
	if err := params.Direction.Validate(); err != nil {
		return Message{}, err
	}

	// Defensive deep-copy before handing to pgx so a caller-side
	// mutation between AppendMessage returning and pgx serializing the
	// param cannot bleed.
	body := make([]byte, len(params.Body))
	copy(body, params.Body)

	id := uuid.New()
	now := r.now().UTC()

	// INSERT ... SELECT FROM k2k_conversations WHERE status='open'
	// short-circuits when the parent conversation is archived; the
	// RETURNING column count drives a 0-row return that we detect via
	// pgx.ErrNoRows. The conversation_id + organization_id filter is
	// redundant with the FK but keeps the planner from short-circuiting
	// to a planner constant.
	const insertSQL = `
INSERT INTO watchkeeper.k2k_messages (
  id, conversation_id, organization_id, sender_watchkeeper_id,
  body, direction, created_at
)
SELECT $1, $2, $3, $4, $5, $6, $7
FROM watchkeeper.k2k_conversations
WHERE id = $2
  AND organization_id = $3
  AND status = 'open'
RETURNING created_at`

	var createdAt time.Time
	err := r.q.QueryRow(
		ctx, insertSQL,
		id, params.ConversationID, params.OrganizationID,
		params.SenderWatchkeeperID, body, string(params.Direction),
		now,
	).Scan(&createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// Either the conversation does not exist (under RLS / for this
		// org) OR it exists but is not open. The peer-tool layer's
		// preceding `Get` distinguishes the two — at this layer we
		// surface `ErrAlreadyArchived` (the canonical "lifecycle
		// terminal" sentinel) and trust the caller stack to translate
		// "no such row" via the `Get` it already ran. Chaining the
		// sentinel with the conversation id keeps the diagnostic
		// readable in logs.
		return Message{}, fmt.Errorf("%w: %s", ErrAlreadyArchived, params.ConversationID)
	}
	if err != nil {
		return Message{}, fmt.Errorf("k2k: append message: %w", err)
	}

	return Message{
		ID:                  id,
		ConversationID:      params.ConversationID,
		OrganizationID:      params.OrganizationID,
		SenderWatchkeeperID: params.SenderWatchkeeperID,
		Body:                body,
		Direction:           params.Direction,
		CreatedAt:           createdAt,
	}, nil
}

// WaitForReply implements [Repository.WaitForReply] via a polling
// loop. The poll interval is configurable via [WithPollInterval]; the
// default is documented on [defaultWaitForReplyPollInterval]. M1.3.a's
// AC pins a LISTEN/NOTIFY follow-up; this adapter ships the polling
// fallback only and the seam remains unchanged for the
// LISTEN/NOTIFY layer.
//
// Expiry is driven by a real [time.NewTimer] rather than comparing
// `r.now()` against a deadline so a fixed-clock test fixture (or a
// production wiring that injects a deterministic `now` for replay
// debugging) cannot pin the wait indefinitely. The repository's
// stamping clock and the wait timeout are intentionally decoupled —
// the stamping clock drives `created_at` only.
func (r *PostgresRepository) WaitForReply(ctx context.Context, conversationID uuid.UUID, since time.Time, timeout time.Duration) (Message, error) {
	if err := ctx.Err(); err != nil {
		return Message{}, err
	}
	if conversationID == uuid.Nil {
		return Message{}, ErrEmptyConversationID
	}
	if timeout <= 0 {
		return Message{}, fmt.Errorf("%w: %s", ErrInvalidWaitTimeout, timeout)
	}

	interval := r.pollInterval
	if interval <= 0 {
		interval = defaultWaitForReplyPollInterval
	}

	pollOnce := func() (Message, bool, error) {
		const selectSQL = `
SELECT id, conversation_id, organization_id, sender_watchkeeper_id,
       body, direction, created_at
FROM watchkeeper.k2k_messages
WHERE conversation_id = $1
  AND direction = 'reply'
  AND created_at > $2
ORDER BY created_at ASC
LIMIT 1`
		var (
			m            Message
			directionStr string
		)
		err := r.q.QueryRow(ctx, selectSQL, conversationID, since.UTC()).Scan(
			&m.ID, &m.ConversationID, &m.OrganizationID, &m.SenderWatchkeeperID,
			&m.Body, &directionStr, &m.CreatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return Message{}, false, nil
		}
		if err != nil {
			return Message{}, false, fmt.Errorf("k2k: wait for reply poll: %w", err)
		}
		m.Direction = MessageDirection(directionStr)
		return m, true, nil
	}

	// First poll: a reply may already be present (race won by the
	// `peer.Reply` writer).
	msg, ok, err := pollOnce()
	if err != nil {
		return Message{}, err
	}
	if ok {
		return msg, nil
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case <-timer.C:
			return Message{}, fmt.Errorf("%w: %s", ErrWaitForReplyTimeout, conversationID)
		case <-ticker.C:
			msg, ok, err := pollOnce()
			if err != nil {
				return Message{}, err
			}
			if ok {
				return msg, nil
			}
		}
	}
}
