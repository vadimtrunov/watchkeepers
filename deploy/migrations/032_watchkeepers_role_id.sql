-- Watchkeeper Keep — watchkeepers.role_id column (Phase 2 §M7.1.a).
--
-- Adds a nullable `role_id` text column to `watchkeeper.watchkeeper` so the
-- M7.1 family ("auto-inheritance on retire→spawn") can correlate a freshly
-- spawned Watchkeeper with the most recent retired Watchkeeper carrying the
-- same role identity. The role_id is derived upstream from a Manifest
-- template slug or an explicit spawn field; this leaf does NOT plumb any
-- writer (legacy callers continue omitting role_id and the column stays
-- NULL). The M7.1.b predecessor-lookup endpoint + M7.1.c
-- NotebookInheritStep saga step land in subsequent leaves and read the
-- column via the partial index introduced here.
--
-- WIRE-SHAPE CONTRACT: free-form text (no shape CHECK). The column is
-- intentionally NOT a FK to any role-catalogue table: Phase 2 has no such
-- catalogue, and a future migration can add one + backfill the FK without
-- rewriting the M7.1.b query path. Treating role_id as an opaque string
-- keeps the column transparent to a downstream catalogue swap and matches
-- how `archive_uri` is stored (migration 022) — opaque text that the
-- application layer pre-validates upstream of the write path.
--
-- INDEX RATIONALE: M7.1.b's predecessor-lookup query reads
--
--   SELECT … FROM watchkeeper.watchkeeper
--   WHERE role_id = $1
--     AND retired_at IS NOT NULL
--     AND archive_uri IS NOT NULL
--   ORDER BY retired_at DESC
--   LIMIT 1
--
-- A partial composite index on `(role_id, retired_at DESC)` that filters
-- exclusively on the retired-with-archive subset (the population the
-- M7.1.b query cares about) keeps the index small — production tables
-- carry orders-of-magnitude more pending/active rows than retired ones,
-- and the M7.1.b query never reaches them. The DESC order on
-- `retired_at` matches the query's ORDER BY so the planner can satisfy
-- the LIMIT 1 with an index-only scan. Mirrors the partial-index
-- precedent of `idx_pending_approvals_open_per_tool` (migration 018) and
-- `idx_outbox_pending` (migration 003) — partial indexes pay for
-- themselves any time the predicate trims more than a couple of
-- percent of the underlying table.
--
-- BACKFILL POSTURE: Phase 2's legacy `watchkeeper.watchkeeper` rows have
-- no documented role_id (the M7.1 family is a Phase 2 introduction);
-- backfilling synthetic values would corrupt the M7.1.b inheritance
-- semantics ("the freshest retired peer for this role"). The column
-- stays NULL for every existing row and the M7.1.c saga step is a
-- no-op when role_id is NULL (a guard the saga owns, not the schema).
-- Pattern: extend a stable schema by adding a nullable column + a
-- partial index sized to the consumer's query shape; let the consumer
-- enforce NOT-NULL on the write paths it owns.
--
-- See `docs/ROADMAP-phase2.md` §M7 → M7.1 → M7.1.a.

-- +goose Up
ALTER TABLE watchkeeper.watchkeeper
ADD COLUMN role_id text NULL;

-- Partial composite index backing the M7.1.b predecessor-lookup query.
-- The WHERE clause matches the query's filter exactly so the index is
-- only populated for retired watchkeepers that have an archive URI —
-- the population the M7.1.b inheritance lookup cares about.
CREATE INDEX idx_watchkeeper_role_id_retired
ON watchkeeper.watchkeeper (role_id, retired_at DESC)
WHERE retired_at IS NOT null AND archive_uri IS NOT null;

-- +goose Down
DROP INDEX IF EXISTS watchkeeper.idx_watchkeeper_role_id_retired;

ALTER TABLE watchkeeper.watchkeeper
DROP COLUMN IF EXISTS role_id;
