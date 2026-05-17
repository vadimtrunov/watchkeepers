-- Watchkeeper Keep — k2k_messages table for the Phase 2 M1.3.a
-- peer.Ask / peer.Reply request-reply primitives.
--
-- Stores one row per message exchanged within a K2K conversation. The
-- two `direction` values (`request`, `reply`) split the
-- `peer.Ask` (inserts a `request`-direction row + blocks until a
-- matching `reply`-direction row appears) from the `peer.Reply`
-- (inserts the `reply`-direction row that unblocks the waiting Ask).
-- The pair satisfies the M1.3.a AC verbatim: "peer.Reply looks up the
-- conversation, appends a `reply`-direction message, signals the
-- waiting Ask".
--
-- Rows are append-only at the Go layer; M1.4 will register an audit
-- subscriber that emits `k2k_message_sent` events keyed off this table.
-- This migration intentionally ships only the storage layer + RLS — no
-- triggers, no LISTEN/NOTIFY hooks (the polling fallback referenced in
-- the M1.3.a AC lives entirely in the Go adapter); M1.3.c will
-- introduce a separate `peer_events` table when the event-stream seam
-- lands.
--
-- Columns:
--   * id                       — opaque PK (uuid).
--   * conversation_id          — FK → watchkeeper.k2k_conversations(id),
--                                ON DELETE RESTRICT so an inadvertent
--                                conversation delete cannot silently
--                                amputate the message chain.
--   * organization_id          — denormalised tenant key. Redundant with
--                                the parent conversation's organization
--                                but stored here so the RLS policy can
--                                match a single column (sibling pattern
--                                to `keepers_log.organization_id` from
--                                migration 003).
--   * sender_watchkeeper_id    — text id of the watchkeeper that
--                                authored the message. Stored as text
--                                (not a uuid FK) so a fake / harness
--                                participant id (matching the
--                                `k2k_conversations.participants` text[]
--                                shape) round-trips without a FK
--                                violation.
--   * body                     — message payload. Stored as `bytea`
--                                because the `peer.Tool` API exposes
--                                `Body []byte` as opaque bytes — a
--                                future caller may legitimately pass
--                                non-UTF-8 / arbitrary binary content
--                                (e.g. a serialised protobuf for an
--                                M1.3.c subscription delivery). A text
--                                column would either reject invalid
--                                UTF-8 inputs at the SQL layer or
--                                silently corrupt them on round-trip;
--                                `bytea` is the binary-safe choice.
--   * direction                — closed-set enum `request | reply`,
--                                CHECK-enforced.
--   * created_at               — wall-clock of insert; defaults to now()
--                                so callers do not need to thread a
--                                timestamp through. Used by
--                                `WaitForReply(since)` as the cursor
--                                anchor.
--
-- RLS shape mirrors migration 029 (k2k_conversations):
--   * ENABLE + FORCE ROW LEVEL SECURITY (FORCE so the migration owner
--     is not silently exempted in production deploys).
--   * One policy per `wk_*_role` keyed off
--     `nullif(current_setting('watchkeeper.org', true), '')::uuid` so
--     an unset GUC evaluates to SQL NULL and the policy fails closed.
--   * INSERT / SELECT grants to the three wk_* roles so the Postgres
--     adapter can drive the table under RLS.
--
-- See `docs/ROADMAP-phase2.md` §M1 → M1.3 → M1.3.a.

-- +goose Up
CREATE TABLE watchkeeper.k2k_messages (
  id uuid PRIMARY KEY,
  conversation_id uuid NOT NULL
  REFERENCES watchkeeper.k2k_conversations (id) ON DELETE RESTRICT,
  organization_id uuid NOT NULL
  REFERENCES watchkeeper.organization (id) ON DELETE RESTRICT,
  sender_watchkeeper_id text NOT NULL CHECK (length(btrim(sender_watchkeeper_id)) > 0),
  body bytea NOT NULL CHECK (length(body) > 0),
  direction text NOT NULL CHECK (direction IN ('request', 'reply')),
  created_at timestamptz NOT NULL DEFAULT now()
);

-- Per-FK index on conversation_id. The hot read path is
-- `WaitForReply(conversation_id, since)`; the composite (conversation_id,
-- created_at) index lets the poller plan a range scan rather than a
-- table scan as message volume grows.
CREATE INDEX k2k_messages_conversation_id_created_at_idx
ON watchkeeper.k2k_messages (conversation_id, created_at);

-- Per-FK index on organization_id supports the RLS planner and any
-- future per-tenant analytic query (e.g. messages-per-org dashboards).
CREATE INDEX k2k_messages_organization_id_idx
ON watchkeeper.k2k_messages (organization_id);

GRANT SELECT, INSERT ON watchkeeper.k2k_messages
TO wk_org_role, wk_user_role, wk_agent_role;

ALTER TABLE watchkeeper.k2k_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.k2k_messages FORCE ROW LEVEL SECURITY;

-- `nullif(current_setting('watchkeeper.org', true), '')::uuid` returns
-- SQL NULL when the GUC is unset (current_setting with the missing-ok
-- flag returns ''). `organization_id = NULL` is never true, so unset
-- GUC is fail-closed — no row passes USING and no row passes WITH
-- CHECK. Identical shape to migration 029's policies.
CREATE POLICY k2k_messages_wk_org_role_policy ON watchkeeper.k2k_messages
FOR ALL TO wk_org_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

CREATE POLICY k2k_messages_wk_user_role_policy ON watchkeeper.k2k_messages
FOR ALL TO wk_user_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

CREATE POLICY k2k_messages_wk_agent_role_policy ON watchkeeper.k2k_messages
FOR ALL TO wk_agent_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

-- +goose Down
DROP POLICY IF EXISTS k2k_messages_wk_agent_role_policy ON watchkeeper.k2k_messages;
DROP POLICY IF EXISTS k2k_messages_wk_user_role_policy ON watchkeeper.k2k_messages;
DROP POLICY IF EXISTS k2k_messages_wk_org_role_policy ON watchkeeper.k2k_messages;

ALTER TABLE watchkeeper.k2k_messages NO FORCE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.k2k_messages DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT ON watchkeeper.k2k_messages
FROM wk_org_role, wk_user_role, wk_agent_role;

DROP INDEX IF EXISTS watchkeeper.k2k_messages_organization_id_idx;
DROP INDEX IF EXISTS watchkeeper.k2k_messages_conversation_id_created_at_idx;

DROP TABLE IF EXISTS watchkeeper.k2k_messages;
