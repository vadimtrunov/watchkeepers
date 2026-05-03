-- Watchkeeper Keep — manifest_version personality/language CHECK constraints
-- (M2.9.a).
--
-- M2.1.a landed the `personality text NULL` and `language text NULL` columns
-- on `watchkeeper.manifest_version` without any validation. This migration
-- adds two CHECK constraints so a malformed value cannot reach the row even
-- if a future handler regresses or a non-Keep writer is ever added:
--
--   * manifest_version_language_format — when language IS NOT NULL it must
--     match BCP 47-lite shape `<lang>(-<REGION>)?`: 2-3 lowercase letters
--     covering ISO 639-1/-3, optionally followed by a 2-letter ISO 3166-1
--     uppercase region (e.g. "en", "en-US", "pt-BR", "kab", "eng").
--   * manifest_version_personality_length — when personality IS NOT NULL it
--     must be at most 1024 unicode codepoints (char_length semantics).
--
-- Both columns remain NULL-allowed; the constraints only fire on a SET value
-- so `omitempty` round-trips on the wire still write SQL NULL. Server-side
-- validation in `parsePutManifestVersionRequest` rejects the same shapes
-- with stable 400 reason codes (`invalid_language`, `personality_too_long`)
-- before the row reaches Postgres; this migration is the defense-in-depth
-- backstop. See `docs/ROADMAP-phase1.md` §M2 → M2.9 → M2.9.a.

-- +goose Up
ALTER TABLE watchkeeper.manifest_version
ADD CONSTRAINT manifest_version_language_format
CHECK (language IS null OR language ~ '^[a-z]{2,3}(-[A-Z]{2})?$');

ALTER TABLE watchkeeper.manifest_version
ADD CONSTRAINT manifest_version_personality_length
CHECK (personality IS null OR char_length(personality) <= 1024);

-- +goose Down
ALTER TABLE watchkeeper.manifest_version
DROP CONSTRAINT IF EXISTS manifest_version_personality_length;

ALTER TABLE watchkeeper.manifest_version
DROP CONSTRAINT IF EXISTS manifest_version_language_format;
