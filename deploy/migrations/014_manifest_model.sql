-- Watchkeeper Keep — manifest_version model column (M5.5.b.b.a).
--
-- Adds a NULL-allowed `model` column to `watchkeeper.manifest_version` so a
-- per-manifest LLM model identifier (e.g. "claude-sonnet-4") can be stored
-- alongside the existing prompt/personality/language fields. The column is
-- length-capped via a CHECK constraint mirroring the precedent in
-- migration 010 (`manifest_version_personality_length`):
--
--   * manifest_version_model_length — when model IS NOT NULL it must be at
--     most 100 unicode codepoints (char_length semantics).
--
-- The column remains NULL-allowed; the constraint only fires on a SET value
-- so `omitempty` round-trips on the wire still write SQL NULL. Server-side
-- validation in `parsePutManifestVersionRequest` rejects oversize values
-- with the stable 400 reason code `model_too_long` before the row reaches
-- Postgres; this migration is the defense-in-depth backstop. See
-- `docs/ROADMAP-phase1.md` §M5 → M5.5 → M5.5.b → M5.5.b.b → M5.5.b.b.a.

-- +goose Up
ALTER TABLE watchkeeper.manifest_version
ADD COLUMN model text NULL;

ALTER TABLE watchkeeper.manifest_version
ADD CONSTRAINT manifest_version_model_length
CHECK (model IS null OR char_length(model) <= 100);

-- +goose Down
ALTER TABLE watchkeeper.manifest_version
DROP CONSTRAINT IF EXISTS manifest_version_model_length;

ALTER TABLE watchkeeper.manifest_version
DROP COLUMN IF EXISTS model;
