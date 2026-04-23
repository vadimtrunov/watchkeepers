-- Watchkeeper Keep — write grants for the three wk_*_role roles (M2.7.d).
--
-- The Keep write endpoints (store / log_append / put_manifest_version) run
-- under `SET LOCAL ROLE wk_<kind>_role` inside a short transaction, so every
-- write path must hold the relevant INSERT grant in addition to the M2.7.b+c
-- SELECT grants from migration 007. `knowledge_chunk` already received
-- `SELECT, INSERT, UPDATE` in migration 005 (its INSERT is additionally
-- gated by RLS WITH CHECK), so this migration only needs to cover the two
-- tables that M2.7.d newly writes from the HTTP surface: `keepers_log`
-- (append-only via trigger; INSERT is unrestricted) and `manifest_version`
-- (no RLS; org-wide authority matrix).
--
-- UPDATE / DELETE are intentionally NOT granted — `keepers_log` enforces
-- append-only via a BEFORE UPDATE / BEFORE DELETE trigger (migration 003),
-- and `manifest_version` is treated as immutable once inserted (new behaviour
-- ships as a new row with a higher `version_no`, unique-keyed on
-- `(manifest_id, version_no)`).

-- +goose Up
GRANT INSERT ON watchkeeper.keepers_log
TO wk_org_role, wk_user_role, wk_agent_role;

GRANT INSERT ON watchkeeper.manifest_version
TO wk_org_role, wk_user_role, wk_agent_role;

-- +goose Down
REVOKE INSERT ON watchkeeper.manifest_version
FROM wk_org_role, wk_user_role, wk_agent_role;

REVOKE INSERT ON watchkeeper.keepers_log
FROM wk_org_role, wk_user_role, wk_agent_role;
