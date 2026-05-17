-- Watchkeeper Keep — manifest_version immutable_core column (M3.1).
--
-- Adds a NULL-allowed `immutable_core` jsonb column to
-- `watchkeeper.manifest_version` so the per-manifest immutable
-- governance object can be stored alongside the existing
-- prompt/personality/language/model/autonomy/notebook fields. Phase 2
-- §M3 calls out five buckets the column carries:
--
--   * role_boundaries — explicit list of capabilities the Watchkeeper
--     is NOT allowed to have, regardless of what self-tuning proposes.
--   * security_constraints — data-handling rules, forbidden data
--     destinations, classification floors.
--   * escalation_protocols — when and to whom to escalate; cannot be
--     disabled.
--   * cost_limits — max token spend per task / day / week.
--   * audit_requirements — what must be logged; cannot be reduced.
--
-- This migration is schema-only — admin-only-editability enforcement
-- lands in M3.2 (handler-layer + RLS) and the self-tuning validator
-- lands in M3.6. The column structure intentionally mirrors the
-- existing `tools` / `authority_matrix` / `knowledge_sources` jsonb
-- treatment: a NULL-allowed `jsonb` column with a defense-in-depth
-- CHECK constraint enforcing top-level shape only (object), so a
-- future bucket extension (M3.4+ tooling: `manifest.merge_fields`,
-- `manifest.rollback`) is a one-line constraint relaxation rather
-- than a column rewrite.
--
--   * manifest_version_immutable_core_shape — when immutable_core IS
--     NOT NULL it must be a JSON object (`jsonb_typeof = 'object'`).
--     Arrays / scalars / null jsonb literals are rejected so a
--     malformed wire payload never reaches the M3.6 validator.
--
-- The column remains NULL-allowed; the constraint only fires on a
-- SET value so `omitempty` round-trips on the wire still write SQL
-- NULL. Server-side validation in `parsePutManifestVersionRequest`
-- rejects non-object payloads with the stable 400 reason code
-- `invalid_immutable_core` before the row reaches Postgres; this
-- migration is the defense-in-depth backstop. See
-- `docs/ROADMAP-phase2.md` §M3 → M3.1.

-- +goose Up
ALTER TABLE watchkeeper.manifest_version
ADD COLUMN immutable_core jsonb NULL;

ALTER TABLE watchkeeper.manifest_version
ADD CONSTRAINT manifest_version_immutable_core_shape
CHECK (immutable_core IS null OR jsonb_typeof(immutable_core) = 'object');

-- +goose Down
ALTER TABLE watchkeeper.manifest_version
DROP CONSTRAINT IF EXISTS manifest_version_immutable_core_shape;

ALTER TABLE watchkeeper.manifest_version
DROP COLUMN IF EXISTS immutable_core;
