-- Watchkeeper Keep — outbox table DDL (M2.1.e).
--
-- `watchkeeper.outbox` is the transactional-outbox staging table. Producers
-- write an event row inside the same transaction that mutates business
-- state; a future publisher worker (M2.7) polls rows where `published_at IS
-- NULL`, fans them out to the message bus, then stamps `published_at`. The
-- partial index `outbox_unpublished_idx` keeps the tail of unpublished
-- events cheap to scan without bloating the index with already-published
-- rows. No FKs — `aggregate_id` references are logical rather than
-- enforced, because outbox rows can outlive their source rows and because
-- the publisher only cares about the `(aggregate_type, aggregate_id,
-- event_type, payload)` envelope.

-- +goose Up
CREATE TABLE watchkeeper.outbox (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  aggregate_type text NOT NULL,
  aggregate_id uuid NOT NULL,
  event_type text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  published_at timestamptz NULL
);

CREATE INDEX outbox_unpublished_idx
ON watchkeeper.outbox (created_at)
WHERE published_at IS null;

-- +goose Down
DROP INDEX IF EXISTS watchkeeper.outbox_unpublished_idx;
DROP TABLE IF EXISTS watchkeeper.outbox;
