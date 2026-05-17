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
-- on `retired_at IS NOT null AND archive_uri IS NOT null AND role_id IS
-- NOT null` keeps the index small — production tables carry orders-of-
-- magnitude more pending/active rows than retired ones, the M7.1.b
-- query never reaches them, and a row whose `role_id` is NULL can never
-- satisfy `WHERE role_id = $1` either (equality on NULL is undefined)
-- so leaving NULL-role rows out of the index removes dead weight
-- without affecting query semantics. The DESC order on `retired_at`
-- matches the query's ORDER BY so the planner can satisfy the LIMIT 1
-- with an index-only scan. Mirrors the partial-index precedent of
-- `idx_pending_approvals_open_per_tool` (migration 018) and
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
-- only populated for retired watchkeepers that have BOTH an archive URI
-- AND a non-NULL role_id — the population the M7.1.b inheritance
-- lookup cares about. NULL-valued role_id rows are excluded explicitly
-- (codex iter-1 finding, Major): the planned lookup is
-- `WHERE role_id = $1` which can never match a SQL-NULL value (equality
-- on NULL is undefined → never satisfied), so including NULL-role rows
-- in the index would bloat it without ever serving a query. Pre-M7.1
-- retired rows + every legacy insert that omits the optional `role_id`
-- body field land with `role_id IS NULL` and therefore stay out of
-- this index, keeping it lean.
CREATE INDEX idx_watchkeeper_role_id_retired
ON watchkeeper.watchkeeper (role_id, retired_at DESC)
WHERE retired_at IS NOT null
AND archive_uri IS NOT null
AND role_id IS NOT null;

-- +goose Down
DROP INDEX IF EXISTS watchkeeper.idx_watchkeeper_role_id_retired;

ALTER TABLE watchkeeper.watchkeeper
DROP COLUMN IF EXISTS role_id;
