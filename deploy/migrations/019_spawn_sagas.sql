-- Watchkeeper Keep — spawn_sagas table for the M7.1.a spawn-saga
-- skeleton. Stores one row per Watchmaster-initiated spawn flow; the
-- M7.1.a Runner state-machine writes `pending` at insert, transitions
-- to `in_flight` per step, and terminal-flips to `completed` or
-- `failed` once the last step returns.
--
-- The matching Go projection is `core/pkg/spawn/saga.Saga` and the
-- DAO contract is `core/pkg/spawn/saga.SpawnSagaDAO`. M7.1.a ships an
-- in-memory implementation only; the Postgres-backed adapter lands in
-- M7.1.b without a migration churn (per the M6.3.b "ship in-memory
-- DAO + tests with consumer" lesson).
--
-- Columns:
--   * id                   — opaque PK (uuid). Minted by the spawn
--                            entrypoint at insert time; the saga
--                            Runner never mints its own id.
--   * manifest_version_id  — the manifest_version this saga is
--                            spawning. Stored at insert and never
--                            mutated by the saga; the eventual
--                            runtime intro step (M7.1.e) reads it back.
--   * status               — `pending` (initial), `in_flight`
--                            (running), `completed` or `failed`
--                            (terminal). CHECK constraint enforces
--                            the closed set; the Runner gates the
--                            transitions and the SQL is a
--                            defense-in-depth.
--   * current_step         — name of the most recently invoked step.
--                            Empty string before the first step runs
--                            and after a zero-step saga completes.
--   * last_error           — failure-reason sentinel (closed-set
--                            snake_case string supplied by the
--                            failing step's typed error chain).
--                            NEVER the underlying error message or
--                            stack trace (M2b.7 PII discipline).
--   * created_at           — when the row was inserted; defaults to
--                            now() so the spawn entrypoint does not
--                            need to thread a timestamp through.
--   * updated_at           — wall-clock of the last state transition;
--                            defaults to now() at insert and is
--                            stamped by the DAO on every write.
--   * completed_at         — NULL while the saga is in-flight;
--                            populated to now() when the row reaches
--                            a terminal state (`completed` or
--                            `failed`).
--
-- The table is intentionally simple — no FK to manifest, no FK to
-- watchkeeper. Like the M6.3.b pending_approvals table, the saga is a
-- state machine over an opaque id, not a join surface. M7.2 (retire
-- saga) and M7.3 (compensations / idempotency) extend the schema; this
-- migration ships the minimum surface M7.1.b can persist against.
--
-- See `docs/ROADMAP-phase1.md` §M7 → M7.1 → M7.1.a.

-- +goose Up
CREATE TABLE watchkeeper.spawn_sagas (
  id uuid PRIMARY KEY,
  manifest_version_id uuid NOT NULL,
  status text NOT NULL DEFAULT 'pending' CHECK (
    status IN ('pending', 'in_flight', 'completed', 'failed')
  ),
  current_step text NOT NULL DEFAULT '',
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz NULL
);

-- +goose Down
DROP TABLE IF EXISTS watchkeeper.spawn_sagas;
