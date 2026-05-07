-- Watchkeeper Keep — manifest_version notebook recall columns (M5.5.c.a).
--
-- Adds two NULL-allowed columns to `watchkeeper.manifest_version` so the
-- per-manifest notebook auto-recall tunables (top_k, relevance_threshold)
-- can be stored alongside the existing prompt/personality/language/model/
-- autonomy fields. Both columns are range-restricted via CHECK constraints
-- mirroring the precedent in migrations 010 / 014 / 015:
--
--   * manifest_version_notebook_top_k_range — when notebook_top_k IS NOT
--     NULL it must satisfy `0 < notebook_top_k <= 100`. Caps absurd K
--     values; NULL (and a wire-level zero, which round-trips to NULL via
--     `intOrNil`) means "auto-recall disabled".
--
--   * manifest_version_notebook_relevance_threshold_range — when
--     notebook_relevance_threshold IS NOT NULL it must satisfy
--     `0 <= notebook_relevance_threshold <= 1`. Relevance is a
--     normalised cosine similarity score; values outside `[0, 1]` are
--     never meaningful.
--
-- The columns remain NULL-allowed; the constraints only fire on a SET
-- value so `omitempty` round-trips on the wire still write SQL NULL.
-- Server-side validation in `parsePutManifestVersionRequest` rejects
-- out-of-range values with the stable 400 reason codes
-- `invalid_notebook_top_k` / `invalid_notebook_relevance_threshold`
-- before the row reaches Postgres; this migration is the
-- defense-in-depth backstop. See
-- `docs/ROADMAP-phase1.md` §M5 → M5.5 → M5.5.c → M5.5.c.a.

-- +goose Up
ALTER TABLE watchkeeper.manifest_version
ADD COLUMN notebook_top_k integer NULL;

ALTER TABLE watchkeeper.manifest_version
ADD CONSTRAINT manifest_version_notebook_top_k_range
CHECK (notebook_top_k IS null OR (notebook_top_k > 0 AND notebook_top_k <= 100));

ALTER TABLE watchkeeper.manifest_version
ADD COLUMN notebook_relevance_threshold real NULL;

ALTER TABLE watchkeeper.manifest_version
ADD CONSTRAINT manifest_version_notebook_relevance_threshold_range
CHECK (
  notebook_relevance_threshold IS null
  OR (notebook_relevance_threshold >= 0 AND notebook_relevance_threshold <= 1)
);

-- +goose Down
ALTER TABLE watchkeeper.manifest_version
DROP CONSTRAINT IF EXISTS manifest_version_notebook_relevance_threshold_range;

ALTER TABLE watchkeeper.manifest_version
DROP COLUMN IF EXISTS notebook_relevance_threshold;

ALTER TABLE watchkeeper.manifest_version
DROP CONSTRAINT IF EXISTS manifest_version_notebook_top_k_range;

ALTER TABLE watchkeeper.manifest_version
DROP COLUMN IF EXISTS notebook_top_k;
