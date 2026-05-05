-- Watchkeeper Keep — human Slack-identity uniqueness + supporting indexes
-- (M4.4).
--
-- M2.1.a created the `watchkeeper.human` table with a nullable
-- `slack_user_id text` column and the `watchkeeper.watchkeeper.lead_human_id
-- uuid NOT NULL` foreign key. M4.4 (Human identity mapping) adds the
-- enforcement layer that lets the messenger adapter map a Slack user ID
-- back to a Keep `human` row deterministically:
--
--   * `human_slack_user_id_key` — UNIQUE on `human(slack_user_id)`. Postgres
--     treats NULLs as distinct in a UNIQUE constraint, so multiple humans
--     with NULL slack_user_id remain allowed; only non-NULL values must be
--     unique. This matches the upstream contract: a Slack user can only
--     ever map to one human row, but a human can exist before being
--     bound to Slack.
--   * `human_slack_user_id_idx` — B-tree partial index keyed on the same
--     column for the `WHERE slack_user_id = $1` lookup pattern the new
--     repository layer issues. The UNIQUE constraint creates an index
--     internally, but spelling out a partial `WHERE slack_user_id IS NOT
--     NULL` index keeps the lookup plan stable across PG major versions
--     (the unique-index-only lookup picks up NULLs that the partial index
--     deliberately excludes).
-- Note: `watchkeeper_lead_human_id_idx` already exists from migration 005
-- (RLS bundle), so the inverse "which watchkeepers does this human lead?"
-- lookup is already covered.
--
-- Grants: writer access on `watchkeeper.human` is granted to the three
-- `wk_*_role` roles so the upcoming POST/PATCH endpoints (M4.4 Go-side)
-- can insert and update human rows under the same SET LOCAL ROLE pattern
-- the watchkeeper handlers already use. SELECT was already granted in
-- migration 007. DELETE remains intentionally not granted — humans are
-- referenced by manifest, watchkeeper, watch_order, and keepers_log; row
-- removal would cascade into FK fallout. See `docs/ROADMAP-phase1.md`
-- §M4 → M4.4.

-- +goose Up
ALTER TABLE watchkeeper.human
ADD CONSTRAINT human_slack_user_id_key UNIQUE (slack_user_id);

CREATE INDEX human_slack_user_id_idx
ON watchkeeper.human (slack_user_id)
WHERE slack_user_id IS NOT null;

GRANT INSERT, UPDATE ON watchkeeper.human
TO wk_org_role, wk_user_role, wk_agent_role;

-- +goose Down
REVOKE INSERT, UPDATE ON watchkeeper.human
FROM wk_org_role, wk_user_role, wk_agent_role;

DROP INDEX IF EXISTS watchkeeper.human_slack_user_id_idx;

ALTER TABLE watchkeeper.human
DROP CONSTRAINT IF EXISTS human_slack_user_id_key;
