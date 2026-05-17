-- Watchkeeper Keep — inherit_digest_runs table for the Phase 2 §M7.1.d
-- 24h inherited-entries digest job.
--
-- Stores one row per organization tracking the most-recent successful
-- digest run. The `core/pkg/notebook/inherit_digest.go` periodic job
-- reads this row on every tick to decide whether the 24h window has
-- elapsed (idempotency guard against duplicate Slack DMs) AND uses
-- `last_window_end` as the strictly-after cursor for the next scan of
-- `keepers_log` rows carrying `event_type = 'notebook_inherited'`.
--
-- The roadmap mention says migration 033, but 033 was already claimed
-- by `033_peer_events.sql` (M1.3.c — shipped between the original
-- roadmap entry's authoring and this leaf's implementation). The next
-- free filename is 034; the goose runner orders by filename so the
-- monotonic file-name sort key is preserved.
--
-- Columns:
--   * organization_id    — tenant key. PRIMARY KEY: one row per org so
--                          the periodic job can `INSERT ... ON CONFLICT
--                          (organization_id) DO UPDATE` to maintain a
--                          single rolling marker per tenant.
--   * last_run_at        — wall-clock of the most-recent successful
--                          `RunInheritDigest` completion. The job
--                          compares `now() - last_run_at >= 24h` as
--                          the "may run" predicate; an earlier tick
--                          within the 24h window is a no-op.
--   * last_window_start  — inclusive lower bound of the last scanned
--                          audit window (= the previous run's
--                          `last_window_end`, or NULL on the first run
--                          when the job seeds with `now() - 24h`).
--   * last_window_end    — exclusive upper bound of the last scanned
--                          audit window. Becomes the next run's
--                          `last_window_start` so the cursor never
--                          rewinds.
--   * created_at         — wall-clock of insert; defaults to now().
--                          Stable across UPDATEs.
--   * updated_at         — wall-clock of the last UPDATE. Maintained
--                          by the application layer (the job sets
--                          this in the same statement as last_run_at).
--
-- RLS shape mirrors migrations 029 / 030 / 033 (k2k_conversations,
-- k2k_messages, peer_events):
--   * ENABLE + FORCE ROW LEVEL SECURITY (FORCE so the migration
--     owner is not silently exempted in production deploys).
--   * One policy per `wk_*_role` keyed off
--     `nullif(current_setting('watchkeeper.org', true), '')::uuid`
--     so an unset GUC evaluates to SQL NULL and the policy fails
--     closed.
--   * SELECT / INSERT / UPDATE grants to the three wk_* roles so
--     the periodic-job process can both read the prior marker and
--     write a fresh one without escalating to a superuser role.
--
-- See `docs/ROADMAP-phase2.md` §M7 → M7.1 → M7.1.d.

-- +goose Up
CREATE TABLE watchkeeper.inherit_digest_runs (
  organization_id uuid PRIMARY KEY
  REFERENCES watchkeeper.organization (id) ON DELETE RESTRICT,
  last_run_at timestamptz NOT NULL,
  last_window_start timestamptz,
  last_window_end timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, UPDATE ON watchkeeper.inherit_digest_runs
TO wk_org_role, wk_user_role, wk_agent_role;

ALTER TABLE watchkeeper.inherit_digest_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.inherit_digest_runs FORCE ROW LEVEL SECURITY;

-- `nullif(current_setting('watchkeeper.org', true), '')::uuid` returns
-- SQL NULL when the GUC is unset (current_setting with the missing-ok
-- flag returns ''). `organization_id = NULL` is never true, so unset
-- GUC is fail-closed — no row passes USING and no row passes WITH
-- CHECK. Identical shape to migrations 029 / 030 / 033.
CREATE POLICY inherit_digest_runs_wk_org_role_policy ON watchkeeper.inherit_digest_runs
FOR ALL TO wk_org_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

CREATE POLICY inherit_digest_runs_wk_user_role_policy ON watchkeeper.inherit_digest_runs
FOR ALL TO wk_user_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

CREATE POLICY inherit_digest_runs_wk_agent_role_policy ON watchkeeper.inherit_digest_runs
FOR ALL TO wk_agent_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

-- +goose Down
DROP POLICY IF EXISTS inherit_digest_runs_wk_agent_role_policy ON watchkeeper.inherit_digest_runs;
DROP POLICY IF EXISTS inherit_digest_runs_wk_user_role_policy ON watchkeeper.inherit_digest_runs;
DROP POLICY IF EXISTS inherit_digest_runs_wk_org_role_policy ON watchkeeper.inherit_digest_runs;

ALTER TABLE watchkeeper.inherit_digest_runs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.inherit_digest_runs DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE ON watchkeeper.inherit_digest_runs
FROM wk_org_role, wk_user_role, wk_agent_role;

DROP TABLE IF EXISTS watchkeeper.inherit_digest_runs;
