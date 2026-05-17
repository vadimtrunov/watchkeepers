-- Watchkeeper Keep — peer_events table + NOTIFY trigger for the
-- Phase 2 M1.3.c `peer.Subscribe` event-stream seam.
--
-- Stores one row per peer-tool event published over the new
-- `peer.EventBus` Publish / Subscribe surface. The M1.4 audit subscriber
-- and any future M5.* tool emitter (`tool_invoked`, `tool_completed`,
-- ...) will INSERT into this table; M1.3.c's `peer.Subscribe` built-in
-- LISTENs on `peer_events` and SELECTs matching rows on every
-- notification.
--
-- Note on numbering: migrations 030 + 031 were already claimed by
-- parallel landings (`030_k2k_messages.sql` + `030_manifest_immutable_core.sql`
-- + `031_k2k_close_summary.sql` + `031_manifest_version_metadata.sql`).
-- This leaf skips to 032 to keep the file-name sort key monotonic with
-- the chronological merge order. The goose runner orders by filename so
-- a stable 032 is the safe choice.
--
-- Columns:
--   * id              — opaque PK (uuid). Minted by the publisher; the
--                       `peer.PostgresEventBus.Publish` flow uses
--                       caller-supplied ids so a future at-least-once
--                       retry can dedup on the PK.
--   * organization_id — denormalised tenant key. Mirrors the
--                       `keepers_log.organization_id` discipline — RLS
--                       policies match a single column so the planner
--                       does not have to join every read.
--   * watchkeeper_id  — text id of the watchkeeper the event pertains
--                       to (the subject, not necessarily the publisher).
--                       The `peer.SubscribeFilter.TargetWatchkeeperID`
--                       filter matches against this column.
--   * event_type      — taxonomy label (e.g. `k2k_message_sent`,
--                       `tool_invoked`). M1.3.c does not pin a finite
--                       enum at the SQL layer because the downstream
--                       consumers (M1.4 audit, M5.* tool emitters) own
--                       their own type strings; a non-empty CHECK keeps
--                       degenerate empty labels out.
--   * payload         — JSONB-encoded event body. Stored as JSONB so a
--                       future indexed-payload query (`payload ->> 'foo'`)
--                       does not require a wholesale rewrite. Default
--                       `'{}'::jsonb` so a publisher that omits the
--                       payload writes a canonical empty object.
--   * created_at      — wall-clock of insert; defaults to now(). The
--                       `peer.SubscribeFilter` drainer uses this as the
--                       `since` cursor for strictly-after delivery.
--
-- Trigger: `peer_event_published` fires AFTER INSERT FOR EACH ROW and
-- emits `NOTIFY peer_events, <id::text>`. The subscriber backend's
-- `WaitForNotification` returns one notification per inserted row; the
-- payload carries the id so a future iteration can SELECT the matching
-- row directly. M1.3.c's drainer ignores the payload + SELECTs every
-- row stamped strictly after its cursor.
--
-- RLS shape mirrors migration 029 (k2k_conversations) and 030
-- (k2k_messages):
--   * ENABLE + FORCE ROW LEVEL SECURITY (FORCE so the migration owner
--     is not silently exempted in production deploys).
--   * One policy per `wk_*_role` keyed off
--     `nullif(current_setting('watchkeeper.org', true), '')::uuid` so
--     an unset GUC evaluates to SQL NULL and the policy fails closed.
--   * INSERT / SELECT grants to the three wk_* roles so the Postgres
--     adapter can drive the table under RLS.
--
-- See `docs/ROADMAP-phase2.md` §M1 → M1.3 → M1.3.c.

-- +goose Up
CREATE TABLE watchkeeper.peer_events (
  id uuid PRIMARY KEY,
  organization_id uuid NOT NULL
  REFERENCES watchkeeper.organization (id) ON DELETE RESTRICT,
  watchkeeper_id text NOT NULL CHECK (length(btrim(watchkeeper_id)) > 0),
  event_type text NOT NULL CHECK (length(btrim(event_type)) > 0),
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Per-FK index on organization_id supports the RLS planner AND the
-- per-tenant drainer's hot read path (SELECT ... WHERE organization_id
-- = $1 AND created_at > $2).
CREATE INDEX peer_events_organization_id_created_at_idx
ON watchkeeper.peer_events (organization_id, created_at);

-- Per-target lookup index. The drainer's filter shape
-- `(organization_id, watchkeeper_id, created_at)` benefits from a
-- composite index when the subscription pins a specific peer target.
CREATE INDEX peer_events_organization_id_watchkeeper_id_idx
ON watchkeeper.peer_events (organization_id, watchkeeper_id, created_at);

GRANT SELECT, INSERT ON watchkeeper.peer_events
TO wk_org_role, wk_user_role, wk_agent_role;

ALTER TABLE watchkeeper.peer_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.peer_events FORCE ROW LEVEL SECURITY;

-- `nullif(current_setting('watchkeeper.org', true), '')::uuid` returns
-- SQL NULL when the GUC is unset (current_setting with the missing-ok
-- flag returns ''). `organization_id = NULL` is never true, so unset
-- GUC is fail-closed — no row passes USING and no row passes WITH
-- CHECK. Identical shape to migrations 029 + 030.
CREATE POLICY peer_events_wk_org_role_policy ON watchkeeper.peer_events
FOR ALL TO wk_org_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

CREATE POLICY peer_events_wk_user_role_policy ON watchkeeper.peer_events
FOR ALL TO wk_user_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

CREATE POLICY peer_events_wk_agent_role_policy ON watchkeeper.peer_events
FOR ALL TO wk_agent_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION watchkeeper.peer_event_published()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  PERFORM pg_notify('peer_events', NEW.id::text);
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER peer_event_published_after_insert
AFTER INSERT ON watchkeeper.peer_events
FOR EACH ROW
EXECUTE FUNCTION watchkeeper.peer_event_published();

-- +goose Down
DROP TRIGGER IF EXISTS peer_event_published_after_insert ON watchkeeper.peer_events;
DROP FUNCTION IF EXISTS watchkeeper.peer_event_published();

DROP POLICY IF EXISTS peer_events_wk_agent_role_policy ON watchkeeper.peer_events;
DROP POLICY IF EXISTS peer_events_wk_user_role_policy ON watchkeeper.peer_events;
DROP POLICY IF EXISTS peer_events_wk_org_role_policy ON watchkeeper.peer_events;

ALTER TABLE watchkeeper.peer_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.peer_events DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT ON watchkeeper.peer_events
FROM wk_org_role, wk_user_role, wk_agent_role;

DROP INDEX IF EXISTS watchkeeper.peer_events_organization_id_watchkeeper_id_idx;
DROP INDEX IF EXISTS watchkeeper.peer_events_organization_id_created_at_idx;

DROP TABLE IF EXISTS watchkeeper.peer_events;
