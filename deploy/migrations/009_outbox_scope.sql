-- Watchkeeper Keep — add scope column to watchkeeper.outbox (M2.7.e.b).
--
-- M2.7.e.a added the in-process publish Registry with exact-scope fan-out but
-- deferred scope derivation on the outbox because the outbox table had no scope
-- column. This migration adds `scope text NOT NULL DEFAULT 'org'` with a CHECK
-- constraint that mirrors the auth-layer scope syntax so the publisher worker
-- (M2.7.e.b) can forward Event.Scope verbatim without a derivation heuristic.
--
-- The CHECK constraint accepts:
--   - 'org'              (org-wide event)
--   - 'user:<uuid>'      (user-scoped event; uuid is RFC 4122 lowercase)
--   - 'agent:<uuid>'     (agent-scoped event; uuid is RFC 4122 lowercase)
--
-- The regex intentionally mirrors the format validated by auth.ParseScope so
-- the outbox and the token-auth layer are governed by the same invariant.

-- +goose Up
ALTER TABLE watchkeeper.outbox
ADD COLUMN scope text NOT NULL DEFAULT 'org';

ALTER TABLE watchkeeper.outbox
ADD CONSTRAINT outbox_scope_check CHECK (
  scope = 'org'
  OR scope ~ '^user:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  OR scope ~ '^agent:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
);

-- +goose Down
ALTER TABLE watchkeeper.outbox DROP CONSTRAINT IF EXISTS outbox_scope_check;
ALTER TABLE watchkeeper.outbox DROP COLUMN IF EXISTS scope;
