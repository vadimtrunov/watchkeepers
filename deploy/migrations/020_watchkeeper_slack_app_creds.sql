-- Watchkeeper Keep — slack_app_creds table for the M7.1.c.a CreateApp
-- saga step. Stores one row per Watchkeeper carrying the OUT-OF-BAND
-- Slack credential bundle (`client_id`, `client_secret`,
-- `signing_secret`, `verification_token`) returned by the underlying
-- `apps.manifest.create` Slack API call. The platform-assigned
-- `app_id` rides ALONGSIDE the credentials as a column rather than as
-- the primary key — the watchkeeper id is the stable saga-row id; the
-- Slack app id can change across re-create scenarios.
--
-- The matching Go projection is `slack.CreateAppCredentials`
-- (core/pkg/messenger/slack/create_app.go) and the DAO contract is
-- `spawn.WatchkeeperSlackAppCredsDAO` (core/pkg/spawn/watchkeeper_creds.go).
-- M7.1.c.a ships an in-memory implementation only; the Postgres-backed
-- adapter lands in a follow-up per the M6.3.b "ship in-memory DAO + tests
-- with consumer" lesson.
--
-- Columns:
--   * watchkeeper_id        — primary key, FK to watchkeeper.watchkeeper.
--                             The stable saga-row id; chosen as PK
--                             because the Slack-assigned app_id can
--                             change across re-create scenarios.
--   * app_id                — Slack-assigned application id returned by
--                             `apps.manifest.create`. Stored as a column
--                             so the credentials bundle has the natural
--                             keying context for downstream OAuth /
--                             install flows.
--   * client_id             — OAuth client_id Slack assigned. Required
--                             at install time (`oauth.v2.access`).
--   * client_secret         — OAuth client_secret. Required at install
--                             time. NEVER written to keepers_log.
--   * verification_token    — Legacy verification token Slack issues
--                             for outbound event verification. Modern
--                             apps verify via the signing secret
--                             instead; included here for completeness.
--   * signing_secret        — Request-signing secret used to verify
--                             inbound Events API / Interactivity
--                             payloads.
--   * created_at            — when the row was inserted; defaults to
--                             now() so the saga step does not need to
--                             thread a timestamp through.
--
-- ENCRYPTION-AT-REST DEFERRAL: every secret column is `text`, NOT
-- `bytea`, because the Phase 1 codebase has no encryption layer. A
-- Phase-2 migration will rotate the columns to encrypted `bytea`
-- alongside the broader secrets-at-rest pass; the DAO contract treats
-- the columns as opaque-bytes-with-extra-steps so the rotation does
-- not churn the consumer surface.
--
-- See `docs/ROADMAP-phase1.md` §M7 → M7.1 → M7.1.c → M7.1.c.a.

-- +goose Up
CREATE TABLE watchkeeper.slack_app_creds (
  watchkeeper_id uuid PRIMARY KEY REFERENCES watchkeeper.watchkeeper (id),
  app_id text NOT NULL,
  client_id text NOT NULL,
  client_secret text NOT NULL,
  verification_token text NOT NULL,
  signing_secret text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS watchkeeper.slack_app_creds;
