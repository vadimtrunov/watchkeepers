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
type PostgresRepository struct {
	q   Querier
	now func() time.Time
}

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
	return &PostgresRepository{q: q, now: now}
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
