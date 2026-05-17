-- Watchkeeper Keep — k2k_conversations table for the Phase 2 M1.1.a
-- Keeper-to-Keeper conversation domain.
--
-- Stores one row per K2K conversation. The conversation is created in
-- `open` state by `core/pkg/k2k.Repository.Open` (in-memory or Postgres
-- impl), populated with the resolved `slack_channel_id` by the M1.1.c
-- lifecycle wiring after the M1.1.b `CreateChannel` returns, advanced
-- by `core/pkg/k2k.Repository.IncTokens` as turns consume tokens, and
-- terminal-flipped to `archived` by `core/pkg/k2k.Repository.Close`
-- (either via operator action or via the M1.6 escalation auto-archive).
--
-- The matching Go projection is `core/pkg/k2k.Conversation` and the
-- DAO contract is `core/pkg/k2k.Repository`. M1.1.a ships both an
-- in-memory and a Postgres-backed implementation; the Postgres adapter
-- is wired in production by the M1.1.c lifecycle helper after this
-- migration applies.
--
-- Columns:
--   * id                   — opaque PK (uuid). Minted by the
--                            Repository.Open call (not by the caller)
--                            so two concurrent Opens never race on a
--                            client-supplied UUID.
--   * organization_id      — tenant key. RLS policies match against
--                            this column via the `watchkeeper.org`
--                            session GUC (sibling to the existing
--                            `watchkeeper.scope`). NOT NULL with a
--                            REFERENCES FK to `watchkeeper.organization`
--                            — a cross-tenant or unknown-org open is
--                            rejected at the FK boundary.
--   * slack_channel_id     — resolved private Slack channel id, NULL
--                            until the M1.1.c lifecycle wiring
--                            populates it from the M1.1.b
--                            `CreateChannel` return value.
--   * participants         — text[] of bot ids invited to the
--                            conversation's Slack channel. NOT NULL
--                            and CHECKed non-empty (a zero-bot
--                            conversation is a degenerate state — at
--                            minimum the requesting bot belongs to it).
--   * subject              — operator-supplied free-text label.
--                            NOT NULL and CHECKed non-empty after
--                            whitespace-trim. Used by M1.4 audit
--                            emission and the M1.1.b channel-name
--                            derivation.
--   * status               — `open` (initial) or `archived` (terminal).
--                            CHECK constraint enforces the closed set;
--                            the Repository surface gates the
--                            transition and the SQL is a defense-in-
--                            depth.
--   * token_budget         — per-conversation token cap (M1.5). 0
--                            disables enforcement; CHECK non-negative.
--   * tokens_used          — running counter, monotonically advanced
--                            by `IncTokens`. CHECK non-negative.
--   * opened_at            — wall-clock of insert; defaults to now()
--                            so callers do not need to thread a
--                            timestamp through.
--   * closed_at            — NULL while open; populated to now() on
--                            archive.
--   * correlation_id       — optional opaque id linking the
--                            conversation to an upstream saga / Watch
--                            Order. NULL when unset.
--   * close_reason         — operator-supplied free-text rationale
--                            for the archive event. Default empty
--                            string while open.
--
-- RLS shape mirrors migration 013 (manifest tenant + RLS):
--   * ENABLE + FORCE ROW LEVEL SECURITY (FORCE so the migration owner
--     is not silently exempted in production deploys).
--   * One policy per `wk_*_role` keyed off
--     `nullif(current_setting('watchkeeper.org', true), '')::uuid` so
--     an unset GUC evaluates to SQL NULL and the policy fails closed
--     (zero rows visible, no INSERT permitted).
--   * INSERT / SELECT / UPDATE grants to the three wk_* roles so the
--     Postgres adapter can drive the table under RLS.
--
-- See `docs/ROADMAP-phase2.md` §M1 → M1.1 → M1.1.a.

-- +goose Up
CREATE TABLE watchkeeper.k2k_conversations (
  id uuid PRIMARY KEY,
  organization_id uuid NOT NULL
  REFERENCES watchkeeper.organization (id) ON DELETE RESTRICT,
  slack_channel_id text NULL,
  participants text [] NOT NULL CHECK (array_length(participants, 1) > 0),
  subject text NOT NULL CHECK (length(btrim(subject)) > 0),
  status text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'archived')),
  token_budget bigint NOT NULL DEFAULT 0 CHECK (token_budget >= 0),
  tokens_used bigint NOT NULL DEFAULT 0 CHECK (tokens_used >= 0),
  opened_at timestamptz NOT NULL DEFAULT now(),
  closed_at timestamptz NULL,
  correlation_id uuid NULL,
  close_reason text NOT NULL DEFAULT ''
);

-- Per-FK index on organization_id. Postgres does not auto-index FK
-- columns; without this, planner cost on joins and ON DELETE RESTRICT
-- checks degrades at scale. Matches the per-FK indexing discipline
-- established in migration 005.
CREATE INDEX k2k_conversations_organization_id_idx
ON watchkeeper.k2k_conversations (organization_id);

-- Partial index supporting the hot "list active conversations" path
-- (the M1.1.c lifecycle wiring + the M1.2 `keepclient.list_peers`
-- integration both filter by status='open' under a per-tenant scope).
-- WHERE status = 'open' keeps the index narrow as the archived backlog
-- grows.
CREATE INDEX k2k_conversations_open_idx
ON watchkeeper.k2k_conversations (organization_id, opened_at)
WHERE status = 'open';

-- Partial index on the optional correlation_id column. Mirrors the
-- `keepers_log_correlation_id` pattern from migration 003 — most rows
-- start with a NULL correlation_id and only the saga-linked subset
-- queries by it.
CREATE INDEX k2k_conversations_correlation_id_idx
ON watchkeeper.k2k_conversations (correlation_id)
WHERE correlation_id IS NOT null;

GRANT SELECT, INSERT, UPDATE ON watchkeeper.k2k_conversations
TO wk_org_role, wk_user_role, wk_agent_role;

ALTER TABLE watchkeeper.k2k_conversations ENABLE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.k2k_conversations FORCE ROW LEVEL SECURITY;

-- `nullif(current_setting('watchkeeper.org', true), '')::uuid` returns
-- SQL NULL when the GUC is unset (current_setting with the missing-ok
-- flag returns ''). `organization_id = NULL` is never true, so unset
-- GUC is fail-closed — no row passes USING and no row passes WITH
-- CHECK. Identical shape to the policies in migration 013.
CREATE POLICY k2k_conversations_wk_org_role_policy ON watchkeeper.k2k_conversations
FOR ALL TO wk_org_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

CREATE POLICY k2k_conversations_wk_user_role_policy ON watchkeeper.k2k_conversations
FOR ALL TO wk_user_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

CREATE POLICY k2k_conversations_wk_agent_role_policy ON watchkeeper.k2k_conversations
FOR ALL TO wk_agent_role
USING (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid)
WITH CHECK (organization_id = nullif(current_setting('watchkeeper.org', true), '')::uuid);

-- +goose Down
DROP POLICY IF EXISTS k2k_conversations_wk_agent_role_policy ON watchkeeper.k2k_conversations;
DROP POLICY IF EXISTS k2k_conversations_wk_user_role_policy ON watchkeeper.k2k_conversations;
DROP POLICY IF EXISTS k2k_conversations_wk_org_role_policy ON watchkeeper.k2k_conversations;

ALTER TABLE watchkeeper.k2k_conversations NO FORCE ROW LEVEL SECURITY;
ALTER TABLE watchkeeper.k2k_conversations DISABLE ROW LEVEL SECURITY;

REVOKE SELECT, INSERT, UPDATE ON watchkeeper.k2k_conversations
FROM wk_org_role, wk_user_role, wk_agent_role;

DROP INDEX IF EXISTS watchkeeper.k2k_conversations_correlation_id_idx;
DROP INDEX IF EXISTS watchkeeper.k2k_conversations_open_idx;
DROP INDEX IF EXISTS watchkeeper.k2k_conversations_organization_id_idx;

DROP TABLE IF EXISTS watchkeeper.k2k_conversations;
