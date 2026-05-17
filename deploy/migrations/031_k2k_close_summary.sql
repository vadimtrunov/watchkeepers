-- Watchkeeper Keep — close_summary column on k2k_conversations for the
-- Phase 2 M1.3.b `peer.Close` lifecycle-finalize primitive.
--
-- Adds a single text column carrying the one-line operator-supplied
-- summary `peer.Close(ctx, conversationID, summary)` writes onto the
-- conversation row alongside the existing `close_reason`. The two
-- columns are intentionally distinct:
--
--   * `close_reason` (migration 029) — the M1.6 escalation auto-archive
--                                      sentinel / M1.7 archive-on-summary
--                                      writer's stable rationale; may be
--                                      a closed-set code.
--   * `close_summary` (this migration) — `peer.Close`'s free-text
--                                      operator-facing summary; carries
--                                      a human-readable one-liner the
--                                      M1.7 archive-on-summary writer
--                                      will later cross-link into the
--                                      Keep knowledge chunk.
--
-- Both columns are NOT NULL with a default empty string so the existing
-- migration-029 INSERT path (and its in-flight prepared statements) keep
-- working unchanged. A close-summary write happens in the
-- `peer.Close` flow AFTER `Lifecycle.Close` archives the row; an
-- archived row whose `close_summary` is the empty string is the
-- canonical "auto-archive without operator summary" state (e.g. an
-- M1.6 escalation timeout).
--
-- Note on numbering: migration 030 was already claimed by two parallel
-- branches that landed simultaneously (`030_k2k_messages.sql` from
-- M1.3.a + `030_manifest_immutable_core.sql` from M3.1). This leaf
-- skips to 031 to keep the file-name sort key monotonic with the order
-- the migrations were merged. The goose / migration runner orders by
-- filename so a stable 031 is the safe choice.
--
-- The matching Go projection extension is
-- `core/pkg/k2k.Conversation.CloseSummary` and the new
-- `Repository.SetCloseSummary` surface; the peer-tool layer's
-- `peer.Close(ctx, conversationID, summary)` is the canonical writer.
--
-- See `docs/ROADMAP-phase2.md` §M1 → M1.3 → M1.3.b.

-- +goose Up
ALTER TABLE watchkeeper.k2k_conversations
ADD COLUMN close_summary text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE watchkeeper.k2k_conversations
DROP COLUMN IF EXISTS close_summary;
