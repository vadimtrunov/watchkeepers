// postgres.go ships the Postgres-backed [Repository] implementation
// the M1.1.c lifecycle wiring will plug in production. The impl is
// intentionally thin: every method maps 1:1 to a single SQL statement
// against the `watchkeeper.k2k_conversations` table created by
// `deploy/migrations/029_k2k_conversations.sql`.
//
// RLS discipline: this adapter does NOT issue `SET LOCAL ROLE` or
// `SET LOCAL watchkeeper.org` itself. The caller — typically the
// `core/internal/keep/db.WithScope` helper — is expected to have
// already established the per-tenant scope on the underlying tx
// BEFORE invoking [PostgresRepository]'s methods. An unset GUC is
// fail-closed at the Postgres layer (zero rows visible, no INSERT
// permitted) per the migration's `nullif(..., ”)::uuid` cast, so a
// misconfigured caller surfaces as `ErrConversationNotFound` (read)
// or an RLS policy violation (write) rather than silent cross-tenant
// access.
//
// Concurrency: every method is safe for concurrent use across
// goroutines on the same [PostgresRepository] value. The underlying
// [pgxpool.Pool] manages the per-call connection lifecycle; the
// `IncTokens` race is handled by Postgres row-level locking via the
// `UPDATE ... WHERE status = 'open' RETURNING tokens_used` shape (a
// concurrent close-then-increment surfaces zero rows and the caller
// receives [ErrAlreadyArchived]).
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
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresRepository is the [Repository] implementation backed by the
// `watchkeeper.k2k_conversations` table. Constructed via
// [NewPostgresRepository]; the zero value is NOT usable — callers must
// always go through the constructor so the underlying pool reference
// is non-nil.
type PostgresRepository struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// Compile-time assertion: [*PostgresRepository] satisfies
// [Repository]. Pins the integration shape so a future change to the
// interface surface fails the build here.
var _ Repository = (*PostgresRepository)(nil)

// NewPostgresRepository returns a Postgres-backed [Repository] over
// the supplied [pgxpool.Pool]. Panics on a nil pool — a nil pool is a
// programmer bug at wiring time, not a runtime error to thread through
// error returns (mirrors the saga step constructors' panic-on-nil-deps
// discipline). The optional `now` argument overrides the wall-clock
// used to stamp `OpenedAt` / `ClosedAt`; pass nil to use [time.Now].
func NewPostgresRepository(pool *pgxpool.Pool, now func() time.Time) *PostgresRepository {
	if pool == nil {
		panic("k2k: NewPostgresRepository: pool must not be nil")
	}
	if now == nil {
		now = time.Now
	}
	return &PostgresRepository{pool: pool, now: now}
}

// Open implements [Repository.Open]. Validation order matches the
// in-memory adapter (ctx → org → subject → participants → budget) so
// the two impls fail-fast on the same inputs with the same sentinels.
// The INSERT uses `RETURNING id, opened_at` so the caller observes
// the server-stamped `OpenedAt` (which may differ from `r.now()` if
// the SQL DEFAULT fires; we still pass `r.now()` for the explicit
// column so a fixture clock in a future integration test can pin the
// value).
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

	const insertSQL = `
INSERT INTO watchkeeper.k2k_conversations (
  id, organization_id, slack_channel_id, participants, subject,
  status, token_budget, tokens_used, opened_at, correlation_id, close_reason
) VALUES (
  $1, $2, NULL, $3, $4, 'open', $5, 0, $6, NULLIF($7, ''), ''
)
RETURNING opened_at`

	var openedAt time.Time
	if err := r.pool.QueryRow(
		ctx, insertSQL,
		id, params.OrganizationID, participants, params.Subject,
		params.TokenBudget, now, params.CorrelationID,
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

	const selectSQL = `
SELECT id, organization_id, COALESCE(slack_channel_id, ''),
       participants, subject, status, token_budget, tokens_used,
       opened_at, COALESCE(closed_at, '0001-01-01 00:00:00+00'::timestamptz),
       COALESCE(correlation_id::text, ''), COALESCE(close_reason, '')
FROM watchkeeper.k2k_conversations
WHERE id = $1`

	var (
		c             Conversation
		closedAt      time.Time
		correlationID string
		status        string
	)
	err := r.pool.QueryRow(ctx, selectSQL, id).Scan(
		&c.ID, &c.OrganizationID, &c.SlackChannelID,
		&c.Participants, &c.Subject, &status, &c.TokenBudget, &c.TokensUsed,
		&c.OpenedAt, &closedAt,
		&correlationID, &c.CloseReason,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, fmt.Errorf("%w: %s", ErrConversationNotFound, id)
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("k2k: get: %w", err)
	}
	c.Status = Status(status)
	if !closedAt.Equal(time.Time{}) && closedAt.Year() > 1 {
		c.ClosedAt = closedAt
	}
	c.CorrelationID = correlationID
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
	const listSQL = `
SELECT id, organization_id, COALESCE(slack_channel_id, ''),
       participants, subject, status, token_budget, tokens_used,
       opened_at, COALESCE(closed_at, '0001-01-01 00:00:00+00'::timestamptz),
       COALESCE(correlation_id::text, ''), COALESCE(close_reason, '')
FROM watchkeeper.k2k_conversations
WHERE ($1 = '' OR status = $1)`

	rows, err := r.pool.Query(ctx, listSQL, string(filter.Status))
	if err != nil {
		return nil, fmt.Errorf("k2k: list: %w", err)
	}
	defer rows.Close()

	var out []Conversation
	for rows.Next() {
		var (
			c             Conversation
			closedAt      time.Time
			correlationID string
			status        string
		)
		if err := rows.Scan(
			&c.ID, &c.OrganizationID, &c.SlackChannelID,
			&c.Participants, &c.Subject, &status, &c.TokenBudget, &c.TokensUsed,
			&c.OpenedAt, &closedAt,
			&correlationID, &c.CloseReason,
		); err != nil {
			return nil, fmt.Errorf("k2k: list scan: %w", err)
		}
		c.Status = Status(status)
		if !closedAt.Equal(time.Time{}) && closedAt.Year() > 1 {
			c.ClosedAt = closedAt
		}
		c.CorrelationID = correlationID
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

	tag, err := r.pool.Exec(ctx, updateSQL, now, reason, id)
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
	if err := r.pool.QueryRow(ctx, probeSQL, id).Scan(&status); err != nil {
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
	err := r.pool.QueryRow(ctx, updateSQL, delta, id).Scan(&tokensUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		// Zero rows — either unknown id or row not open. Disambiguate
		// via a follow-up SELECT, matching [Close]'s discipline.
		const probeSQL = `SELECT status FROM watchkeeper.k2k_conversations WHERE id = $1`
		var status string
		if probeErr := r.pool.QueryRow(ctx, probeSQL, id).Scan(&status); probeErr != nil {
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
