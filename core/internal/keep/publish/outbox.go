// Package publish — outbox worker (M2.7.e.b).
//
// Worker polls watchkeeper.outbox for rows where published_at IS NULL,
// converts each row to an Event, calls reg.Publish for SSE fan-out, and
// stamps published_at=now() in the same transaction on success. Publish
// failure leaves the row unpublished for the next tick.
//
// Observability (M10.1): operator-facing diagnostics go through a
// wired-in [*slog.Logger] from [core/pkg/wklog]; per-batch publish
// counters go through [core/pkg/wkmetrics]. Both surfaces are
// optional — a worker built without [Worker.WithObservability] silently
// drops log lines and no-ops metric calls. Iter-1 review fix:
// [Worker.WithObservability] MUST be called BEFORE [Worker.Run]; the
// fields are not synchronised for re-binding during a live Run.
//
// The worker runs with the database session role of the pool owner (the
// Postgres user in KEEP_DATABASE_URL) rather than a wk_*_role; outbox rows
// are infrastructure, not user-scope data, and the owner already has full
// privileges on watchkeeper.outbox. This avoids adding a dedicated publisher
// role (which would be migration 010 and broader scope) while keeping the
// privilege surface minimal: only SELECT + UPDATE on watchkeeper.outbox.
package publish

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vadimtrunov/watchkeepers/core/pkg/wkmetrics"
)

// OutboxRow is the in-memory representation of one watchkeeper.outbox row
// as read by the worker. Exported so tests can build values directly without
// opening a Postgres connection.
type OutboxRow struct {
	ID            uuid.UUID
	AggregateType string
	AggregateID   uuid.UUID
	EventType     string
	Payload       json.RawMessage
	Scope         string
	CreatedAt     time.Time
}

// RowToEvent converts an OutboxRow to a publish.Event ready for fan-out.
// Exported for unit tests; production code uses it inside the worker tick.
func RowToEvent(row OutboxRow) Event {
	return Event{
		ID:            row.ID,
		Scope:         row.Scope,
		AggregateType: row.AggregateType,
		AggregateID:   row.AggregateID,
		EventType:     row.EventType,
		Payload:       row.Payload,
		CreatedAt:     row.CreatedAt,
	}
}

// WorkerConfig holds the tunable parameters for the outbox worker.
// Production code reads these from config.Config; tests build the struct
// directly.
type WorkerConfig struct {
	// PollInterval is the duration between successive scans of
	// watchkeeper.outbox for unpublished rows.
	PollInterval time.Duration
}

// Worker polls watchkeeper.outbox and fans events out via a Publisher.
// Construct with NewWorker (production) or NewWorkerWithPublisher (tests).
//
// Observability fields (logger, metrics) are optional and may be nil.
// A nil logger drops lines; a nil metrics instance is contractually
// safe per wkmetrics' nil-receiver semantics. Production main.go
// always calls [Worker.WithObservability] BEFORE [Worker.Run]; tests
// that do not care leave them unset.
type Worker struct {
	pool    *pgxpool.Pool
	pub     Publisher
	cfg     WorkerConfig
	logger  *slog.Logger
	metrics *wkmetrics.Metrics
}

// NewWorker creates a Worker backed by the given pool and Registry. cfg
// supplies the poll interval from KEEP_OUTBOX_POLL_INTERVAL.
func NewWorker(pool *pgxpool.Pool, reg *Registry, cfg WorkerConfig) *Worker {
	return &Worker{pool: pool, pub: reg, cfg: cfg}
}

// NewWorkerWithPublisher creates a Worker with an arbitrary Publisher
// implementation. Intended for tests that substitute a stub publisher
// without starting a real Registry.
func NewWorkerWithPublisher(pool *pgxpool.Pool, pub Publisher, cfg WorkerConfig) *Worker {
	return &Worker{pool: pool, pub: pub, cfg: cfg}
}

// WithObservability attaches structured-logging and metrics surfaces to
// the worker.
//
// Iter-1 review (M3): this MUST be called BEFORE [Worker.Run]. The
// fields are not synchronised for re-binding during a live Run — the
// goroutine started by [Worker.Run] reads them on every tick, so a
// concurrent WithObservability would race. The doc-comment used to
// advertise "Calling it more than once replaces previous bindings"; that
// invitation was retracted because it papered over a real foot-gun.
// Passing nil for either argument is allowed and means "do not emit
// for this surface".
func (w *Worker) WithObservability(logger *slog.Logger, metrics *wkmetrics.Metrics) {
	w.logger = logger
	w.metrics = metrics
}

// Run starts the outbox polling loop. It ticks at cfg.PollInterval, selects
// all unpublished rows FOR UPDATE SKIP LOCKED (safe for future multi-replica
// scale-out), calls pub.Publish for each, and stamps published_at=now() in
// the same transaction on success. A failed Publish leaves the row for the
// next tick. Run returns nil on clean shutdown (ctx cancelled) and a non-nil
// error only on unexpected failure.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.tick(ctx); err != nil {
				// Log but do not return: the worker keeps running on
				// transient DB errors. A permanent failure will surface
				// on every tick via the log.
				w.logError(ctx, "outbox worker tick error", err)
			}
		}
	}
}

// logError emits an ERROR log via the wired logger and is a no-op when
// no logger was supplied. Centralised so the worker has exactly one
// place that touches the optional surface — easier to audit, easier to
// extend.
//
// Iter-1 review (m2): uses [slog.Logger.LogAttrs] so the call site
// stays zero-alloc for the common path; the prior []any conversion
// forced one boxing allocation per attribute.
func (w *Worker) logError(ctx context.Context, msg string, err error, attrs ...slog.Attr) {
	if w.logger == nil {
		return
	}
	full := make([]slog.Attr, 0, 1+len(attrs))
	full = append(full, slog.String("error", err.Error()))
	full = append(full, attrs...)
	w.logger.LogAttrs(ctx, slog.LevelError, msg, full...)
}

// tick performs one poll: select unpublished rows, publish, stamp.
//
// Iter-1 review (C2 / codex P1b): the success counter is incremented
// ONLY AFTER tx.Commit returns nil. The previous shape bumped
// `outcome=success` per-row inside the loop, before Commit; a
// subsequent stamp failure (return err → deferred Rollback) or a
// Commit failure itself would silently inflate the success counter
// for rows that never landed in the DB. The next tick would re-publish
// the same rows and re-inflate the counter. The fix collects the
// successful row IDs in a slice during the loop and bumps the counter
// only on the happy-Commit path. On any error path the batch counts
// as one outcome=error sample so operators see the tick failure
// without per-row over-counting.
func (w *Worker) tick(ctx context.Context) error {
	tx, err := w.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT id, aggregate_type, aggregate_id, event_type, payload, scope, created_at
		FROM watchkeeper.outbox
		WHERE published_at IS NULL
		ORDER BY created_at ASC
		FOR UPDATE SKIP LOCKED
	`)
	if err != nil {
		return err
	}

	var batch []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err := rows.Scan(
			&r.ID,
			&r.AggregateType,
			&r.AggregateID,
			&r.EventType,
			&r.Payload,
			&r.Scope,
			&r.CreatedAt,
		); err != nil {
			rows.Close()
			return err
		}
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// stampedSuccessfully tracks rows whose Publish + stamp both
	// succeeded INSIDE the transaction. The success counter is bumped
	// for these rows ONLY after Commit returns nil (see below).
	stampedSuccessfully := 0
	for _, row := range batch {
		ev := RowToEvent(row)
		if err := w.pub.Publish(ctx, ev); err != nil {
			// Publish failure (e.g. context cancelled during shutdown):
			// log inline, count this row's failure once, leave it for
			// the next tick. Don't return — other rows can still try.
			w.logError(ctx, "outbox worker publish failed", err, slog.String("row_id", row.ID.String()))
			w.metrics.RecordOutboxPublished(wkmetrics.OutcomeError)
			continue
		}
		if _, err := tx.Exec(
			ctx,
			`UPDATE watchkeeper.outbox SET published_at = now() WHERE id = $1`,
			row.ID,
		); err != nil {
			// Stamp failure: surface the error to Run via return so the
			// transaction rolls back. Iter-1 fix (m4): do NOT log here;
			// the Run loop logs the tick error once at outbox.go's
			// `case <-ticker.C` branch. Counting this row as error here
			// would also double-count if the Run loop chose to count
			// the tick failure separately — keep the metric increment
			// at the batch-level error site below by letting Commit's
			// caller drive it.
			return err
		}
		stampedSuccessfully++
	}

	if err := tx.Commit(ctx); err != nil {
		// Commit failure: every row in stampedSuccessfully is now
		// unstamped in the DB (the Rollback in defer reverted them).
		// Count one batch-level error so operators see the failure
		// without per-row over-counting.
		w.logError(ctx, "outbox worker commit failed", err)
		w.metrics.RecordOutboxPublished(wkmetrics.OutcomeError)
		return err
	}
	// Happy path: bump success counter once per row whose stamp landed
	// in the committed transaction.
	for i := 0; i < stampedSuccessfully; i++ {
		w.metrics.RecordOutboxPublished(wkmetrics.OutcomeSuccess)
	}
	return nil
}
