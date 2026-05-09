-- Watchkeeper Keep — install-tokens columns on slack_app_creds for the
-- M7.1.c.b.b OAuthInstall saga step. The step exchanges the operator-
-- supplied OAuth code (Phase-1 admin-grant flow; future M7.x will
-- automate via callback HTTP route) for a bot/user token bundle via
-- Slack's `oauth.v2.access` and persists the tokens into the existing
-- `watchkeeper.slack_app_creds` row keyed by watchkeeper_id.
--
-- The matching Go projection is `slack.InstallTokens`
-- (core/pkg/messenger/slack/install_app.go) and the saga consumer is
-- `spawn.OAuthInstallStep` (core/pkg/spawn/oauthinstall_step.go). The
-- DAO contract `spawn.WatchkeeperSlackAppCredsDAO` (extended via a new
-- `PutInstallTokens` method) is the consumer-facing seam.
--
-- ENCRYPTION-AT-REST CONTRACT: `bot_access_token`, `user_access_token`,
-- and `refresh_token` carry AES-GCM ciphertexts produced by
-- `core/pkg/secrets.Encrypter` (M7.1.c.b.a primitive). The wire format
-- is `nonce(12) || sealed`, where `sealed` is the AEAD output
-- (ciphertext || 16-byte tag). Plaintext is NEVER persisted in any of
-- these columns. An empty token (e.g. `refresh_token` when rotation is
-- disabled on the app manifest) is stored as SQL NULL — NOT as the
-- 28-byte ciphertext of the empty string — so downstream `len() == 0`
-- checks remain authoritative.
--
-- Columns added (all NULL until install completes; the row itself is
-- created by the M7.1.c.a CreateAppStep before this step runs):
--   * bot_access_token       — AES-GCM ciphertext of the `xoxb-*` bot
--                              user token returned by `oauth.v2.access`.
--                              Always non-NULL after a successful install.
--   * user_access_token      — AES-GCM ciphertext of the `xoxp-*` user
--                              token Slack returns when the authorising
--                              user granted user-scope OAuth permissions.
--                              NULL when the install requested only bot
--                              scopes.
--   * refresh_token          — AES-GCM ciphertext of the rotation refresh
--                              token Slack returns when token rotation is
--                              enabled on the app manifest. NULL when
--                              rotation is disabled.
--   * bot_token_expires_at   — UTC expiry of `bot_access_token` derived
--                              from the `expires_in` response field.
--                              NULL when rotation is disabled (token
--                              never expires).
--   * installed_at           — UTC timestamp the OAuth exchange completed
--                              and the row was updated. NULL on rows that
--                              have only the M7.1.c.a CreateApp creds.
--
-- See `docs/ROADMAP-phase1.md` §M7 → M7.1 → M7.1.c → M7.1.c.b → M7.1.c.b.b.

-- +goose Up
ALTER TABLE watchkeeper.slack_app_creds
ADD COLUMN bot_access_token bytea NULL;

ALTER TABLE watchkeeper.slack_app_creds
ADD COLUMN user_access_token bytea NULL;

ALTER TABLE watchkeeper.slack_app_creds
ADD COLUMN refresh_token bytea NULL;

ALTER TABLE watchkeeper.slack_app_creds
ADD COLUMN bot_token_expires_at timestamptz NULL;

ALTER TABLE watchkeeper.slack_app_creds
ADD COLUMN installed_at timestamptz NULL;

-- +goose Down
ALTER TABLE watchkeeper.slack_app_creds
DROP COLUMN IF EXISTS installed_at;

ALTER TABLE watchkeeper.slack_app_creds
DROP COLUMN IF EXISTS bot_token_expires_at;

ALTER TABLE watchkeeper.slack_app_creds
DROP COLUMN IF EXISTS refresh_token;

ALTER TABLE watchkeeper.slack_app_creds
DROP COLUMN IF EXISTS user_access_token;

ALTER TABLE watchkeeper.slack_app_creds
DROP COLUMN IF EXISTS bot_access_token;
