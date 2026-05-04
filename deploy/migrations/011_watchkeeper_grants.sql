-- Watchkeeper Keep — watchkeeper-table CRUD grants for the three wk_*_role
-- roles (M3.2.a).
--
-- The Keep watchkeeper-resource endpoints (insert/update_status/get/list) run
-- under `SET LOCAL ROLE wk_<kind>_role` inside a short transaction. Migration
-- 007 already granted `SELECT` on `watchkeeper.watchkeeper`; this migration
-- adds `INSERT, UPDATE` so the new server handlers can write the row at
-- create time and stamp `spawned_at` / `retired_at` on the status transitions
-- (`pending → active`, `active → retired`).
--
-- DELETE is intentionally NOT granted — watchkeeper rows are append-only at
-- the Keep API surface; retirement is a status transition, not a row removal.
--
-- Documented limitation: row-level security on `watchkeeper.watchkeeper` is
-- NOT enabled at this milestone. Every authenticated caller (regardless of
-- scope) can see and mutate every watchkeeper row. A future migration will
-- add an RLS policy keyed off the same `app.scope` GUC the existing
-- knowledge_chunk policy uses; until then, the GRANT statements below are
-- the entire access policy. See docs/ROADMAP-phase1.md §M3 → M3.2 → M3.2.a.

-- +goose Up
GRANT INSERT, UPDATE ON watchkeeper.watchkeeper
TO wk_org_role, wk_user_role, wk_agent_role;

-- +goose Down
REVOKE INSERT, UPDATE ON watchkeeper.watchkeeper
FROM wk_org_role, wk_user_role, wk_agent_role;
