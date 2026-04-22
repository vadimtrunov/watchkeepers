-- Watchkeeper Keep — knowledge_chunk table with pgvector HNSW index (M2.1.c).
--
-- `watchkeeper.knowledge_chunk` stores embedded knowledge fragments that the
-- future `search` handler (M2.7) will query via cosine-distance KNN. Each row
-- carries a `scope` column seeded ahead of the M2.1.d RLS migration — rows
-- are either organisation-wide (`'org'`) or bound to a user or agent subject
-- (`'user:<uuid>'` / `'agent:<uuid>'`). The `vector(1536)` column width
-- matches the OpenAI `text-embedding-3-small` model and is fixed at schema
-- time; a later milestone can widen or parameterise it via a new migration.
-- An HNSW index on `embedding vector_cosine_ops` (m = 16, ef_construction =
-- 64) keeps KNN lookups logarithmic at the row counts we expect for Phase 1.
-- Per the M2.1.a lesson, Down drops the table only — NOT the `vector`
-- extension, because extensions are database-scoped and future migrations
-- may depend on it.

-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE watchkeeper.knowledge_chunk (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  scope text NOT NULL DEFAULT 'org' CHECK (
    scope = 'org' OR scope LIKE 'user:%' OR scope LIKE 'agent:%'
  ),
  subject text NULL,
  content text NOT NULL, -- noqa: RF04
  embedding vector(1536) NOT NULL,
  tool_version text NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX knowledge_chunk_embedding_hnsw_idx
ON watchkeeper.knowledge_chunk
USING hnsw (embedding vector_cosine_ops)
WITH (m = 16, ef_construction = 64);

-- +goose Down
DROP TABLE IF EXISTS watchkeeper.knowledge_chunk;
