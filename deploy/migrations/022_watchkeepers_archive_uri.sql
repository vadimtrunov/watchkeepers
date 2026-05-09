-- Watchkeeper Keep — archive_uri column on watchkeepers for the M7.2.c
-- MarkRetired saga step. The step is the second concrete step of the
-- M7.2 retire saga (after M7.2.b NotebookArchive); it persists the
-- archive URI returned by `notebook.ArchiveOnRetire` onto the
-- watchkeeper row so a future operator can locate the archived
-- notebook tarball without re-reading the saga audit chain.
--
-- The matching Go projection is `keepclient.Watchkeeper.ArchiveURI`
-- (core/pkg/keepclient/read_watchkeeper.go) and the saga consumer is
-- `spawn.MarkRetiredStep` (core/pkg/spawn/markretired_step.go). The
-- consumer-facing seam is `keepclient.Client.UpdateWatchkeeperRetired`,
-- a NEW method that hits the existing
-- `PATCH /v1/watchkeepers/{id}/status` endpoint with the optional
-- `archive_uri` body field; the existing `UpdateWatchkeeperStatus`
-- method continues to drive the `pending→active` transition (which
-- has no archive_uri).
--
-- WIRE-SHAPE CONTRACT: `archive_uri` is a free-form storage URI string
-- (RFC 3986 with non-empty scheme — `file:///…`, `s3://…`, `gs://…`,
-- or test fakes). The keep server enforces non-empty + non-blank when
-- the body field is supplied; the saga step pre-validates the URI
-- shape before it ever reaches the wire (see M7.2.b
-- `ErrInvalidArchiveURI`). Storing as `text NULL` rather than a
-- structured (scheme, key) pair keeps the column transparent to a
-- future archivestore-backend swap and matches the way
-- `notebook.archived_uri` is logged in the M2b.7 audit row.
--
-- Column added (NULL until the M7.2.c MarkRetired step transitions
-- the row to `retired`; existing pre-M7.2 retire flows wrote NULL by
-- definition):
--   * archive_uri            — non-empty storage URI of the archived
--                              notebook tarball, or NULL when the row
--                              has not yet been retired (or was retired
--                              before M7.2.c shipped).
--
-- The watchkeeper row's status state machine remains unchanged:
--   pending → active   (no archive_uri permitted; rejected pre-tx)
--   active  → retired  (optional archive_uri; persisted when supplied)
-- A NULL archive_uri on a retired row is permitted — it preserves
-- backward-compatibility with rows retired by the M6.2.c synchronous
-- tool before the M7.2 saga family landed and through any future
-- compensator path that retires without an archive (M7.3 scope).
--
-- See `docs/ROADMAP-phase1.md` §M7 → M7.2 → M7.2.c.

-- +goose Up
ALTER TABLE watchkeeper.watchkeeper
ADD COLUMN archive_uri text NULL;

-- +goose Down
ALTER TABLE watchkeeper.watchkeeper
DROP COLUMN IF EXISTS archive_uri;
