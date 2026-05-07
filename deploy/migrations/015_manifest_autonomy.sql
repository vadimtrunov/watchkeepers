-- Watchkeeper Keep — manifest_version autonomy column (M5.5.b.c.a).
--
-- Adds a NULL-allowed `autonomy` column to `watchkeeper.manifest_version`
-- so the per-manifest supervision regime (matching `runtime.AutonomyLevel`
-- — see `core/pkg/runtime/runtime.go:33-48`) can be stored alongside the
-- existing prompt/personality/language/model fields. The column is
-- enum-restricted via a CHECK constraint mirroring the precedent in
-- migration 010 (`manifest_version_language_format`):
--
--   * manifest_version_autonomy_enum — when autonomy IS NOT NULL it must
--     be one of `'supervised'`, `'autonomous'`. Empty `''` round-trips
--     from the wire as SQL NULL via `stringOrNil(body.Autonomy)` and
--     defaults to supervised at runtime; the constraint only fires on a
--     non-NULL set value so omitempty round-trips on the wire still
--     write SQL NULL.
--
-- The column remains NULL-allowed; the constraint only fires on a SET
-- value. Server-side validation in `parsePutManifestVersionRequest`
-- rejects out-of-set values with the stable 400 reason code
-- `invalid_autonomy` before the row reaches Postgres; this migration is
-- the defense-in-depth backstop. See
-- `docs/ROADMAP-phase1.md` §M5 → M5.5 → M5.5.b → M5.5.b.c → M5.5.b.c.a.

-- +goose Up
ALTER TABLE watchkeeper.manifest_version
ADD COLUMN autonomy text NULL;

ALTER TABLE watchkeeper.manifest_version
ADD CONSTRAINT manifest_version_autonomy_enum
CHECK (autonomy IS null OR autonomy IN ('supervised', 'autonomous'));

-- +goose Down
ALTER TABLE watchkeeper.manifest_version
DROP CONSTRAINT IF EXISTS manifest_version_autonomy_enum;

ALTER TABLE watchkeeper.manifest_version
DROP COLUMN IF EXISTS autonomy;
