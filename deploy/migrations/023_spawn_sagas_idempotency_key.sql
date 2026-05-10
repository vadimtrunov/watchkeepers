-- Watchkeeper Keep — idempotency_key column on spawn_sagas for the
-- M7.3.a robustness slice. The column carries the upstream
-- idempotency token that the spawn / retire kickoffer derives from
-- the approval_token (or any future per-call dedup key); it is the
-- primary defense against retried approvals double-creating Slack
-- Apps, runtimes, or notebook files.
--
-- The matching Go projection is `saga.Saga.IdempotencyKey`
-- (core/pkg/spawn/saga/dao.go) and the producer-side seam is
-- `saga.SpawnSagaDAO.InsertIfAbsent` (NEW in M7.3.a). The legacy
-- `Insert(ctx, id, manifestVersionID)` continues to write NULL into
-- the column so rows persisted before M7.3.a (or by zero-step
-- smoke-test wiring that does not need dedup) keep their
-- pre-extension shape.
--
-- WIRE-SHAPE CONTRACT: `idempotency_key` is a free-form non-empty
-- string when supplied (the project convention is `tok-<uuid>` to
-- match approval_token framing, but the column is opaque to the
-- column itself); empty AND whitespace-only strings are rejected by
-- the DAO surface (`ErrEmptyIdempotencyKey`) BEFORE the row reaches
-- the wire so a `"   "` sentinel cannot smuggle a bypass past the
-- UNIQUE index. Storing as `text NULL` rather than `text NOT NULL
-- DEFAULT ''` keeps the partial UNIQUE-WHERE-NOT-NULL semantics
-- intact: a NULL row never collides with another NULL row, so legacy
-- callers that pre-date M7.3.a stay wire-compatible.
--
-- SECURITY: `idempotency_key` carries the FULL `approval_token`
-- bearer verbatim (the kickoffer derives the key from the token
-- 1:1 so the partial UNIQUE index dedups the approval flow). The
-- audit chain redacts to the `tok-XXXXXX` prefix per the M6.3.b
-- token-prefix-display lesson, but psql access to this row sees the
-- full bearer. Access discipline matches `pending_approvals.token`
-- (see migration 018); both columns are operator-only.
--
-- Column added (NULL for legacy rows, non-empty for M7.3.a callers):
--   * idempotency_key       — opaque dedup key minted by the kickoffer
--                             from the approval_token (or any future
--                             per-call dedup source). NULL when the
--                             row was inserted by the legacy
--                             `Insert` path; non-empty (and globally
--                             unique) when inserted via
--                             `InsertIfAbsent`.
--
-- IDEMPOTENCY SEMANTICS: a partial UNIQUE index (`WHERE idempotency_key
-- IS NOT NULL`) enforces dedup ONLY for non-NULL values. Postgres
-- semantics: a non-partial UNIQUE index over a NULLable column
-- already treats NULLs as distinct (the M2 keep-side
-- `human_slack_user_id_key` in migration 012 leans on this directly),
-- so a regular UNIQUE would functionally suffice; the partial form
-- is chosen here so the dedup intent is explicit at the index
-- definition rather than implicit in Postgres-native NULL semantics.
-- The DAO contract (`InsertIfAbsent` returns the existing row on
-- conflict, never an error) is the consumer surface; the partial
-- index is the storage-layer guarantee that two concurrent approvals
-- cannot race past it.
--
-- Additional column added in this migration to back the M7.3.a
-- replay-payload contract (codex iter-1 Major + critic iter-1
-- Major K2):
--   * watchkeeper_id        — the saga's target watchkeeper id (=
--                             [saga.SpawnContext.AgentID] in Go).
--                             Stored on the row at insert time so a
--                             replayed-event payload can emit the
--                             FIRST-call's watchkeeperID instead of
--                             the discarded second-call candidate.
--                             NULL for legacy rows inserted via
--                             `Insert` (zero-step smoke wiring) or
--                             pre-M7.3.a-deployed sagas.
--
-- DATA INTEGRITY: no CHECK constraint pins the key to a status (unlike
-- the M7.2.c `archive_uri` column which is retired-only). The
-- idempotency_key is meaningful at insert time — it is the saga's
-- "we have committed to running this exactly-once" marker — and stays
-- on the row through every state transition (`pending` →
-- `in_flight` → `completed`/`failed`) so a future replay correctly
-- short-circuits regardless of where the original saga is in its
-- lifecycle. Removing the key on a state transition would re-open the
-- dedup gap on completed-but-replayed approvals.
--
-- See `docs/ROADMAP-phase1.md` §M7 → M7.3 → M7.3.a.

-- +goose Up
ALTER TABLE watchkeeper.spawn_sagas
ADD COLUMN IF NOT EXISTS idempotency_key text NULL;

ALTER TABLE watchkeeper.spawn_sagas
ADD COLUMN IF NOT EXISTS watchkeeper_id uuid NULL;

CREATE UNIQUE INDEX IF NOT EXISTS spawn_sagas_idempotency_key_uniq
ON watchkeeper.spawn_sagas (idempotency_key)
WHERE idempotency_key IS NOT null;

-- +goose Down
-- Down-migration drops both the index and the data-bearing columns.
-- Intentional: rows inserted via the M7.3.a `InsertIfAbsent` path
-- carry forward-only data (the bearer token + target watchkeeper id);
-- a deployment that rolls back past M7.3.a no longer needs the
-- dedup contract those columns served. Operators who need to
-- preserve the data across a rollback should snapshot the table
-- before running this down-migration.
DROP INDEX IF EXISTS watchkeeper.spawn_sagas_idempotency_key_uniq;

ALTER TABLE watchkeeper.spawn_sagas
DROP COLUMN IF EXISTS watchkeeper_id;

ALTER TABLE watchkeeper.spawn_sagas
DROP COLUMN IF EXISTS idempotency_key;
