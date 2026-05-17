-- Watchkeeper Keep — manifest_version metadata columns (Phase 2 §M3.3).
--
-- Adds three explicit metadata columns to `watchkeeper.manifest_version`
-- so the audit / rollback / proposal trail the Phase 2 §M3 conversational
-- retune flow needs is mechanical rather than implied by `created_at`
-- timestamp ordering. Phase 1 left these implicit (the row's `created_at`
-- + the per-manifest ascending `version_no` were enough to reconstruct
-- the history); Phase 2 §M3.4 `manifest.history` / `manifest.diff` /
-- `manifest.rollback` / `manifest.merge_fields` tools and the §M3.5
-- Slack UX BOTH need a stable, queryable surface for "why was this
-- version proposed" + "what version is it derived from" + "who
-- proposed it", and neither can be reconstructed from the existing
-- columns alone.
--
-- This migration is schema-only — admin-only-editability enforcement
-- of `previous_version_id` rewrites (M3.2) and the rollback /
-- merge-fields tools that consume the columns (M3.4) land in
-- subsequent leaves. The migration mirrors the M3.1 immutable_core
-- approach: NULL-allowed columns with shape CHECK constraints, no
-- backfill of legacy rows (Phase 1 manifests have neither a documented
-- reason nor a proposer; backfilling synthetic values would corrupt
-- the audit story). Pattern: extend a stable schema by adding nullable
-- metadata columns + a defense-in-depth CHECK constraint per
-- shape-invariant; let M3.4 tooling enforce non-NULL on the write
-- paths it owns.
--
--   * reason — free-text rationale carried by every manifest_version
--     row created through an M3.4 tool (or any future propose-style
--     flow). Nullable so Phase 1 + M3.1 rows that predate this
--     migration stay valid. Capped at 1024 Unicode codepoints
--     (`char_length` semantics; matches the existing `personality`
--     cap from migration 010) so a runaway proposer payload cannot
--     blow out the row + the audit page that surfaces it.
--   * previous_version_id — uuid FK to `watchkeeper.manifest_version(id)`,
--     declaring the version this row is derived from. Nullable for the
--     root version of every manifest (there is no previous). The FK
--     uses the Postgres default `ON DELETE NO ACTION` (effectively
--     RESTRICT) — once a row's `previous_version_id` is populated,
--     the target row CANNOT be deleted without first rewriting the
--     pointer. This is intentional for an immutable audit chain;
--     conversational-rollback flows (M3.5) write NEW rows naming the
--     target as `previous_version_id`, never UPDATE / DELETE existing
--     rows. A `manifest_version_previous_version_self_ref` CHECK
--     rejects a row pointing at itself (impossible by FK timing on
--     INSERT but a future UPDATE path could try it).
--
--     Same-manifest invariant: the FK does NOT constrain the target
--     row's `manifest_id` (uuid FKs are single-column by Postgres
--     design). A composite FK + composite UNIQUE on
--     `(id, manifest_id)` would push the invariant into the schema,
--     but M3.3 keeps the schema change minimal and enforces it at
--     the write-path layer instead (the
--     `handlePutManifestVersion` INSERT carries a
--     `NOT EXISTS-implies-row-rejected` gate so a caller cannot
--     anchor a manifest_version at a previous_version_id whose
--     `manifest_id` does not match `$1`; cross-tenant rejection
--     surfaces as `pgx.ErrNoRows → 404 not_found`). A future
--     migration MAY tighten this to a composite FK once the M3.4
--     tools' write-paths share a single gate.
--   * proposer — free-text identifier of the actor that proposed this
--     version. Not a uuid FK to either `human` or `watchkeeper`
--     because the M3.4 tools take callers from heterogeneous sources
--     (a human handle, a Watchkeeper UUID, the literal string
--     "watchmaster" for system-initiated rollback proposals). A
--     polymorphic FK would force a discriminator column the M3.4
--     surface does not need. Capped at 256 Unicode codepoints —
--     enough room for any UUID + tag prefix + slack handle without
--     leaving an unbounded-text foot-gun.
--
-- Pattern reference: matches the M5.5.b.* manifest_version column-
-- extension chain (model in 014, autonomy in 015, notebook recall in
-- 016, immutable_core in 030). Each migration ships an `ALTER TABLE …
-- ADD COLUMN … CHECK` block with one stable reason code per CHECK
-- so the server-side `parsePutManifestVersionRequest` precheck can
-- surface a clear 400 reason before the row hits Postgres.

-- +goose Up
ALTER TABLE watchkeeper.manifest_version
ADD COLUMN reason text NULL;

ALTER TABLE watchkeeper.manifest_version
ADD COLUMN previous_version_id uuid NULL
REFERENCES watchkeeper.manifest_version (id);

ALTER TABLE watchkeeper.manifest_version
ADD COLUMN proposer text NULL;

ALTER TABLE watchkeeper.manifest_version
ADD CONSTRAINT manifest_version_reason_length
CHECK (reason IS null OR char_length(reason) <= 1024);

ALTER TABLE watchkeeper.manifest_version
ADD CONSTRAINT manifest_version_proposer_length
CHECK (proposer IS null OR char_length(proposer) <= 256);

ALTER TABLE watchkeeper.manifest_version
ADD CONSTRAINT manifest_version_previous_version_self_ref
CHECK (previous_version_id IS null OR previous_version_id <> id);

-- Index the FK column so M3.4 `manifest.history` reverse-walks
-- (find rows derived from a given previous_version_id) and FK
-- maintenance scans stay efficient. Mirrors the
-- `manifest_created_by_human_id_idx` precedent (migration 005).
CREATE INDEX manifest_version_previous_version_id_idx
ON watchkeeper.manifest_version (previous_version_id);

-- +goose Down
DROP INDEX IF EXISTS watchkeeper.manifest_version_previous_version_id_idx;

ALTER TABLE watchkeeper.manifest_version
DROP CONSTRAINT IF EXISTS manifest_version_previous_version_self_ref;

ALTER TABLE watchkeeper.manifest_version
DROP CONSTRAINT IF EXISTS manifest_version_proposer_length;

ALTER TABLE watchkeeper.manifest_version
DROP CONSTRAINT IF EXISTS manifest_version_reason_length;

ALTER TABLE watchkeeper.manifest_version
DROP COLUMN IF EXISTS proposer;

ALTER TABLE watchkeeper.manifest_version
DROP COLUMN IF EXISTS previous_version_id;

ALTER TABLE watchkeeper.manifest_version
DROP COLUMN IF EXISTS reason;
