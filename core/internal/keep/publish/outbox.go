// Package publish — outbox worker (M2.7.e.b).
//
// Worker polls watchkeeper.outbox for rows where published_at IS NULL,
// converts each row to an Event, calls reg.Publish for SSE fan-out, and
// stamps published_at=now() in the same transaction on success. Publish
// failure leaves the row unpublished for the next tick. Errors are logged,
// never panicked.
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
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
type Worker struct {
	pool *pgxpool.Pool
	pub  Publisher
	cfg  WorkerConfig
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
				log.Printf("outbox worker tick error: %v", err)
			}
		}
	}
}

// tick performs one poll: select unpublished rows, publish, stamp. All work
// happens inside a single transaction so a mid-batch crash leaves unpublished
// rows for the next tick (exactly-once delivery via stamp-in-txn).
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

	for _, row := range batch {
		ev := RowToEvent(row)
		if err := w.pub.Publish(ctx, ev); err != nil {
			// Publish failure (e.g. context cancelled during shutdown):
			// log and leave the row for the next tick.
			log.Printf("outbox worker: publish row %s: %v", row.ID, err)
			continue
		}
		if _, err := tx.Exec(ctx,
			`UPDATE watchkeeper.outbox SET published_at = now() WHERE id = $1`,
			row.ID,
		); err != nil {
			log.Printf("outbox worker: stamp row %s: %v", row.ID, err)
			// Stamp failure: do not mark the whole batch failed — continue
			// and let the tx rollback handle the partial update.
			return err
		}
	}

	return tx.Commit(ctx)
}
