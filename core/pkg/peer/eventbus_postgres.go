// eventbus_postgres.go ships the Postgres-backed [EventBus]
// implementation. The M1.3.c AC pins a `peer_events` table behind a
// `peer_event_published` trigger that fires `NOTIFY peer_events` so a
// listening backend wakes up + drains the new rows.
//
// Implementation shape:
//
//   - Publish: thin INSERT into `watchkeeper.peer_events`. The
//     [Querier] seam mirrors the M1.1.a [k2k.PostgresRepository]
//     discipline — the production wiring threads a per-request
//     [pgx.Tx] in scope under `SET LOCAL ROLE` + `SET LOCAL
//     watchkeeper.org` so RLS scopes the write to the caller's tenant.
//   - Subscribe: requires a dedicated long-lived backend connection
//     (LISTEN bind survives only on the connection that issued it).
//     The adapter takes a [PoolAcquirer] seam — production wiring
//     passes a `*pgxpool.Pool`; tests can inject a fake. On Subscribe,
//     the adapter acquires a connection, issues `LISTEN peer_events`,
//     and spawns a goroutine that drains `WaitForNotification` into
//     the caller-facing bounded channel. ctx cancel + CancelFunc both
//     stop the goroutine + release the connection back to the pool.
//   - On every notification the goroutine SELECTs the matching rows
//     (filter pinned to OrganizationID + TargetWatchkeeperID +
//     EventTypes; the `since` cursor is the last-seen `created_at` so
//     a notify lost on the wire degrades to "next notify catches up").
//     Slow-consumer drop policy mirrors the in-memory adapter:
//     non-blocking send + bus-wide atomic counter increment.
//
// The adapter is intentionally compile-tested at M1.3.c and exercised
// at runtime by the `scripts/migrate-schema-test.sh` block (ab). The
// end-to-end LISTEN/NOTIFY drainer's integration test is owned by the
// M1.4 audit subscriber (which is the first production publisher).

package peer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the minimum pgx surface the [PostgresEventBus] Publish
// path needs. Mirrors [k2k.Querier] exactly so production wiring can
// pass the same per-tx [pgx.Tx] verbatim. Hoisted here (instead of
// importing the k2k alias) to keep the peer package's compile-time
// dependency graph narrow — the peer-tool layer already imports k2k
// for the conversation-storage seam; adding a second indirection for
// the event-bus seam would couple the two packages tighter than the
// layering warrants.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Notification is the minimum surface the adapter needs from a Postgres
// `NOTIFY` payload. Mirrors `pgconn.Notification.Payload` and `Channel`
// so a test fake can synthesise notifications without standing up a
// real Postgres + LISTEN backend.
type Notification struct {
	// Channel is the `LISTEN` channel the notification arrived on. The
	// adapter only LISTENs on `peer_events` so this field is constant
	// in production; surfaced for completeness so a future multi-channel
	// listener does not have to re-shape the seam.
	Channel string
	// Payload is the optional `NOTIFY <channel>, <payload>` body. The
	// migration's trigger emits the event id so the listener can SELECT
	// the matching row directly instead of polling.
	Payload string
}

// ListenerConn is the narrow seam the [PostgresEventBus] consumes for
// the long-lived LISTEN connection. Pinned to a method set so a test
// fake can synthesise notifications + replay row reads without standing
// up a real Postgres backend. Production wiring satisfies it with a
// wrapper over `*pgxpool.Conn` whose `Conn()` exposes `*pgx.Conn`.
type ListenerConn interface {
	// Exec runs a SQL command that returns no rows (used for `LISTEN
	// peer_events`).
	Exec(ctx context.Context, sql string, args ...any) error

	// WaitForNotification blocks until the next NOTIFY arrives, the ctx
	// cancels, or the connection drops. Mirrors
	// `*pgx.Conn.WaitForNotification` minus the pgconn return type.
	WaitForNotification(ctx context.Context) (Notification, error)

	// Query runs the matching SELECT used to drain new rows when a
	// NOTIFY fires. Production passes through to `*pgx.Conn.Query`.
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)

	// Release returns the connection to the pool (in production a
	// `*pgxpool.Conn.Release`).
	Release()
}

// PoolAcquirer is the narrow seam the [PostgresEventBus] consumes to
// acquire a long-lived backend connection for LISTEN traffic. Pinned to
// a single method so test wiring can inject a hand-rolled fake. Mirrors
// the `*pgxpool.Pool.Acquire` shape; the production wiring satisfies it
// with a thin wrapper that wraps the returned `*pgxpool.Conn` in a
// `ListenerConn`.
type PoolAcquirer interface {
	Acquire(ctx context.Context) (ListenerConn, error)
}

// PostgresEventBus is the Postgres-backed [EventBus] adapter. Construct
// via [NewPostgresEventBus]; the zero value is NOT usable.
type PostgresEventBus struct {
	q        Querier
	acquirer PoolAcquirer
	bufSize  int

	dropped atomic.Uint64
}

// PostgresEventBusOption configures a [PostgresEventBus]. Constructed
// via the `WithXxx` helpers.
type PostgresEventBusOption func(*PostgresEventBus)

// WithPostgresBufferSize overrides the per-subscription bounded buffer
// size. Mirrors [WithMemoryBufferSize]. Non-positive values are
// silently coerced back to [defaultMemoryBufferSize].
func WithPostgresBufferSize(size int) PostgresEventBusOption {
	return func(b *PostgresEventBus) {
		if size > 0 {
			b.bufSize = size
		}
	}
}

// NewPostgresEventBus returns a configured [PostgresEventBus] ready for
// Publish / Subscribe traffic. Both seams are required (non-nil); the
// constructor panics otherwise (mirrors [NewTool]'s panic-on-nil
// discipline). The [Querier] handles the INSERT side and runs under the
// caller's per-tx `SET LOCAL ROLE` / `SET LOCAL watchkeeper.org`; the
// [PoolAcquirer] handles the long-lived LISTEN connection.
func NewPostgresEventBus(q Querier, acquirer PoolAcquirer, opts ...PostgresEventBusOption) *PostgresEventBus {
	if q == nil {
		panic("peer: NewPostgresEventBus: q must not be nil")
	}
	if acquirer == nil {
		panic("peer: NewPostgresEventBus: acquirer must not be nil")
	}
	b := &PostgresEventBus{
		q:        q,
		acquirer: acquirer,
		bufSize:  defaultMemoryBufferSize,
	}
	for _, o := range opts {
		o(b)
	}
	return b
}

// Publish implements [EventBus.Publish] against the
// `watchkeeper.peer_events` table. The migration's
// `peer_event_published` trigger fires `NOTIFY peer_events` on every
// successful INSERT so every listening subscriber wakes up + drains the
// row.
func (b *PostgresEventBus) Publish(ctx context.Context, event Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if event.ID == uuid.Nil {
		return ErrInvalidEventID
	}
	if event.OrganizationID == uuid.Nil {
		return ErrInvalidOrganizationID
	}
	if strings.TrimSpace(event.WatchkeeperID) == "" {
		return ErrEmptyWatchkeeperID
	}
	if strings.TrimSpace(event.EventType) == "" {
		return ErrEmptyEventType
	}

	payload := clonePayload(event.Payload)
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	// Empty / nil payload coerces to the canonical empty JSON object so
	// the `payload jsonb NOT NULL DEFAULT '{}'::jsonb` column does not
	// surface a CHECK / NOT NULL violation on a publisher that omits the
	// body. Mirrors the M9.4.a `handlers_write.handleLogAppend` "default
	// empty object" discipline.
	payloadStr := string(payload)
	if len(payload) == 0 {
		payloadStr = "{}"
	}

	const insertSQL = `
INSERT INTO watchkeeper.peer_events (
  id, organization_id, watchkeeper_id, event_type, payload, created_at
) VALUES ($1, $2, $3, $4, $5::jsonb, $6)`
	if _, err := b.q.Exec(
		ctx, insertSQL,
		event.ID, event.OrganizationID, event.WatchkeeperID, event.EventType,
		payloadStr, event.CreatedAt,
	); err != nil {
		return fmt.Errorf("peer: publish event: %w", err)
	}
	return nil
}

// Subscribe implements [EventBus.Subscribe]. Acquires a dedicated
// backend connection, issues `LISTEN peer_events`, and spawns a
// drainer goroutine that selects matching rows on every notification.
func (b *PostgresEventBus) Subscribe(ctx context.Context, filter SubscribeFilter) (<-chan Event, CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if filter.OrganizationID == uuid.Nil {
		return nil, nil, ErrInvalidOrganizationID
	}

	// Defensive deep-copy of the event-type filter so caller-side
	// mutation cannot bleed into the goroutine's matcher.
	copied := SubscribeFilter{
		OrganizationID:      filter.OrganizationID,
		TargetWatchkeeperID: filter.TargetWatchkeeperID,
	}
	if len(filter.EventTypes) > 0 {
		copied.EventTypes = make([]string, len(filter.EventTypes))
		copy(copied.EventTypes, filter.EventTypes)
	}

	conn, err := b.acquirer.Acquire(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("peer: subscribe acquire conn: %w", err)
	}
	if err := conn.Exec(ctx, "LISTEN peer_events"); err != nil {
		conn.Release()
		return nil, nil, fmt.Errorf("peer: subscribe LISTEN: %w", err)
	}

	out := make(chan Event, b.bufSize)
	subCtx, cancelFn := context.WithCancel(ctx)
	var closeOnce sync.Once
	closeOut := func() { closeOnce.Do(func() { close(out) }) }

	cancel := func() {
		cancelFn()
	}

	go b.drain(subCtx, conn, copied, out, closeOut)

	return out, cancel, nil
}

// drain runs the per-subscription drainer goroutine. Loops on
// WaitForNotification → SELECT new rows → non-blocking deliver. Exits
// when ctx cancels, the connection drops, or a non-transient SQL error
// fires.
//
// Cursor discipline (codex iter-1 P1+P2 fix): a wall-clock-only cursor
// like `time.Now()` can permanently miss events under two realistic
// scenarios: (1) the publisher's transaction commits at a Postgres
// `now()` that is older than the drainer's app-side `time.Now()`
// (long-running tx) or older than this process's wall-clock (clock
// skew across hosts), so the SELECT filter `created_at > $since` drops
// the row; (2) two concurrent publish transactions stamp the same
// `now()` timestamp (Postgres `now()` is transaction-scoped, not
// statement-scoped) and the strictly-greater filter drops the
// later-inserted-but-equal-timestamp row.
//
// Fix shape: track delivered rows via a `(created_at, id)` tuple
// cursor — the SELECT uses `(created_at, id) > ($cursor_ts, $cursor_id)`
// so two rows sharing a `created_at` are ordered by id and neither is
// dropped. Initial cursor reads `MAX(created_at)` from the table once
// after LISTEN binds: any row committed before LISTEN is observable to
// a fresh SELECT, so the initial cursor is the row anchor at LISTEN
// time. A row that commits AFTER LISTEN but with `created_at < MAX`
// (long-running tx) is still drained on the next notification because
// its id will be strictly greater than the cursor id at the matching
// timestamp.
func (b *PostgresEventBus) drain(ctx context.Context, conn ListenerConn, filter SubscribeFilter, out chan<- Event, closeOut func()) {
	defer func() {
		conn.Release()
		closeOut()
	}()

	// Initial cursor: the largest persisted `created_at` (and the
	// matching id at that timestamp). A row committed before LISTEN is
	// observable via the synchronous prime-the-cursor SELECT; the
	// SELECT's anchor becomes the strict-greater cursor for every
	// subsequent notification poll. If the table is empty, anchor at
	// (epoch, uuid.Nil) so the first real row is strictly greater.
	cursorTs, cursorID, err := b.primeCursor(ctx, conn, filter.OrganizationID)
	if err != nil {
		return
	}

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		_, err := conn.WaitForNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			// Transient connection loss / drop: terminate the
			// subscription. The caller observes a closed channel and
			// can re-subscribe. We do not attempt to reconnect on the
			// subscriber's behalf — that policy belongs to the caller.
			return
		}

		// Drain every matching row strictly after the `(cursorTs,
		// cursorID)` tuple. A single notification may correspond to
		// multiple rows if a burst of inserts collapsed.
		newTs, newID, err := b.deliverSince(ctx, conn, filter, cursorTs, cursorID, out)
		if err != nil {
			return
		}
		cursorTs = newTs
		cursorID = newID
	}
}

// primeCursor reads `(MAX(created_at), id at that timestamp)` from
// `peer_events` for the supplied tenant so the drainer's initial
// cursor anchors on the latest persisted row at LISTEN-bind time. A
// drainer that starts against an empty table returns `(epoch,
// uuid.Nil)` so the first real row is strictly greater on the
// `(ts, id) > (cursor)` comparator.
func (b *PostgresEventBus) primeCursor(ctx context.Context, conn ListenerConn, orgID uuid.UUID) (time.Time, uuid.UUID, error) {
	const sql = `
SELECT created_at, id
FROM watchkeeper.peer_events
WHERE organization_id = $1
ORDER BY created_at DESC, id DESC
LIMIT 1`
	rows, err := conn.Query(ctx, sql, orgID)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("peer: subscribe prime cursor: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var ts time.Time
		var id uuid.UUID
		if err := rows.Scan(&ts, &id); err != nil {
			return time.Time{}, uuid.Nil, fmt.Errorf("peer: subscribe prime cursor scan: %w", err)
		}
		return ts.UTC(), id, nil
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("peer: subscribe prime cursor rows: %w", err)
	}
	// Empty table: anchor at the zero time + zero uuid so the first
	// real row is strictly greater on the tuple comparator.
	return time.Time{}, uuid.Nil, nil
}

// deliverSince SELECTs every matching row strictly after `(cursorTs,
// cursorID)` and non-blocking-delivers each onto `out`. Returns the
// new largest `(created_at, id)` tuple so the caller advances the
// cursor.
//
// The tuple-cursor shape `(created_at, id) > ($ts, $id)` is the codex
// iter-1 P2 fix: two rows sharing a `created_at` (Postgres `now()` is
// transaction-scoped, so two concurrent inserts may legitimately
// stamp the same timestamp) are ordered by id, and neither is dropped.
// The strict-greater operator on the tuple is well-defined per the
// SQL standard — Postgres evaluates lexicographically across the
// composite.
func (b *PostgresEventBus) deliverSince(ctx context.Context, conn ListenerConn, filter SubscribeFilter, cursorTs time.Time, cursorID uuid.UUID, out chan<- Event) (time.Time, uuid.UUID, error) {
	rows, err := conn.Query(ctx, buildSelectSQL(filter), buildSelectArgs(filter, cursorTs, cursorID)...)
	if err != nil {
		return cursorTs, cursorID, fmt.Errorf("peer: subscribe drain query: %w", err)
	}
	defer rows.Close()

	newTs := cursorTs
	newID := cursorID
	for rows.Next() {
		var ev Event
		if err := rows.Scan(
			&ev.ID, &ev.OrganizationID, &ev.WatchkeeperID, &ev.EventType,
			&ev.Payload, &ev.CreatedAt,
		); err != nil {
			return newTs, newID, fmt.Errorf("peer: subscribe drain scan: %w", err)
		}
		ev.CreatedAt = ev.CreatedAt.UTC()
		// Advance the tuple cursor: ORDER BY created_at ASC, id ASC
		// guarantees the last-seen row is the new high-water mark.
		newTs = ev.CreatedAt
		newID = ev.ID
		// Defensive deep-copy of the payload on egress so a consumer
		// mutating the slice cannot bleed across replays.
		deliver := ev
		deliver.Payload = clonePayload(ev.Payload)
		select {
		case <-ctx.Done():
			return newTs, newID, ctx.Err()
		case out <- deliver:
		default:
			b.dropped.Add(1)
		}
	}
	if err := rows.Err(); err != nil {
		return newTs, newID, fmt.Errorf("peer: subscribe drain rows: %w", err)
	}
	return newTs, newID, nil
}

// buildSelectSQL constructs the SELECT statement used by the drainer.
// The base statement filters by organization_id + the `(created_at, id)`
// tuple cursor; the optional TargetWatchkeeperID and EventTypes filter
// portions are dynamically appended.
//
// Tuple-cursor SQL: `(created_at, id) > ($2, $3)` is the codex iter-1
// P2 fix — see [deliverSince] for the rationale.
func buildSelectSQL(filter SubscribeFilter) string {
	var sb strings.Builder
	sb.WriteString(`SELECT id, organization_id, watchkeeper_id, event_type, payload, created_at
FROM watchkeeper.peer_events
WHERE organization_id = $1
  AND (created_at, id) > ($2, $3)`)
	argIdx := 4
	if filter.TargetWatchkeeperID != "" {
		fmt.Fprintf(&sb, "\n  AND watchkeeper_id = $%d", argIdx)
		argIdx++
	}
	if len(filter.EventTypes) > 0 {
		fmt.Fprintf(&sb, "\n  AND event_type = ANY($%d)", argIdx)
	}
	sb.WriteString("\nORDER BY created_at ASC, id ASC")
	return sb.String()
}

// buildSelectArgs mirrors [buildSelectSQL]: returns the matching arg
// slice in the same order.
func buildSelectArgs(filter SubscribeFilter, cursorTs time.Time, cursorID uuid.UUID) []any {
	args := []any{filter.OrganizationID, cursorTs, cursorID}
	if filter.TargetWatchkeeperID != "" {
		args = append(args, filter.TargetWatchkeeperID)
	}
	if len(filter.EventTypes) > 0 {
		args = append(args, filter.EventTypes)
	}
	return args
}

// DroppedEvents implements [EventBus.DroppedEvents].
func (b *PostgresEventBus) DroppedEvents() uint64 {
	return b.dropped.Load()
}
