package notebook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// promoteEventType is the `event_type` column written to keepers_log for
// the per-PromoteToKeep audit event emitted by [DB.PromoteToKeep] when a
// [Logger] has been wired in via [WithLogger]. Held as a const so tests
// pin against the same string the production code emits.
//
// Note: this is the *proposal* stage event; the future "actually written
// into watchkeeper.knowledge_chunk" event lands in M6.2 under a separate
// type name (`notebook_promoted_to_keep`) — they are not collapsed because
// downstream subscribers want to distinguish "agent wants to share X" from
// "Watchmaster approved and persisted X".
const promoteEventType = "notebook_promotion_proposed"

// Scope constants for [Proposal.Scope]. Mirrors the CHECK constraint on
// `watchkeeper.knowledge_chunk.scope` (deploy/migrations/004_knowledge_chunk.sql):
// rows are either organisation-wide (`"org"`) or bound to a user / agent
// subject via the `"user:<uuid>"` / `"agent:<uuid>"` prefixes.
//
// `ScopeOrg` is exported as a value because the AC pins the default; the
// agent / user prefix forms are documented as helpers below — callers
// build them with [fmt.Sprintf] or string concatenation since the suffix
// is a runtime UUID, not a compile-time constant.
const (
	// ScopeOrg is the default scope assigned to a fresh [Proposal] and
	// matches the `'org'` literal in the Keep schema CHECK constraint.
	ScopeOrg = "org"

	// ScopeUserPrefix is the prefix for user-scoped knowledge chunks.
	// Callers build a full scope string as `ScopeUserPrefix + "<uuid>"`.
	ScopeUserPrefix = "user:"

	// ScopeAgentPrefix is the prefix for agent-scoped knowledge chunks.
	// Callers build a full scope string as `ScopeAgentPrefix + "<uuid>"`.
	ScopeAgentPrefix = "agent:"
)

// Proposal is the Notebook-side packaging of a single Notebook entry into a
// shape ready for Watchmaster approval and (subsequently, in M6.2) for
// insertion into `watchkeeper.knowledge_chunk` on the Keep server. The
// first four fields mirror `knowledge_chunk` columns one-to-one (Subject,
// Content, Embedding, ToolVersion); the remaining seven carry provenance
// the proposal stage needs to correlate the proposal back to its source
// Notebook entry, the agent that proposed it, and the scope under which
// it would be visible.
//
// `Scope` defaults to [ScopeOrg]. Callers wishing to propose a narrower
// scope set `ScopeUserPrefix + "<uuid>"` or `ScopeAgentPrefix + "<uuid>"`
// after construction.
//
// `SourceCreatedAt` is a copy of the source entry's `created_at` (epoch
// ms) so the eventual Keep-side row carries the original moment of
// observation, not the moment of promotion. `ProposedAt` is the moment
// of promotion (epoch ms) — a distinct datum the audit log uses to
// reconstruct timelines.
type Proposal struct {
	// Subject mirrors `entry.subject` and the future `knowledge_chunk.subject`.
	// Optional; empty string means absent.
	Subject string

	// Content is the textual body lifted from `entry.content`. Required,
	// non-empty (mirrors the source entry's NOT NULL constraint).
	Content string

	// Embedding is the float32 vector lifted from the entry's
	// `entry_vec` row. Length is always [EmbeddingDim].
	Embedding []float32

	// ToolVersion mirrors `entry.tool_version` and the future
	// `knowledge_chunk.tool_version`. Nullable — preserved as `*string`
	// so a present-but-empty value (rare) stays distinct from absent.
	ToolVersion *string

	// ProposalID is a freshly-minted UUID v7 stamped at proposal time.
	// Distinct from `NotebookEntryID` so a single Notebook entry can be
	// proposed multiple times (e.g. retries after a Watchmaster reject)
	// with each proposal individually addressable in the audit log.
	ProposalID string

	// AgentID is the UUID of the agent owning the source notebook.
	// Identical to the `agent_id` column the Keep server stamps on the
	// `keepers_log` row produced by the audit emit.
	AgentID string

	// NotebookEntryID is the UUID v7 of the source `entry` row. Stable
	// across proposals; correlates this proposal back to the local
	// notebook for auditors and (later) Watchmaster operators.
	NotebookEntryID string

	// Category mirrors the source entry's category — one of the five
	// values in [categoryEnum]. Carried into the proposal so downstream
	// classifiers can shard / route without re-reading the local DB.
	Category string

	// Scope governs the visibility of the (eventually-persisted) Keep
	// row. Defaults to [ScopeOrg]; see [ScopeUserPrefix] /
	// [ScopeAgentPrefix] for the user / agent variants.
	Scope string

	// SourceCreatedAt is `entry.created_at` (epoch ms) — the moment the
	// source observation was first remembered.
	SourceCreatedAt int64

	// ProposedAt is `time.Now().UnixMilli()` at proposal time.
	ProposedAt int64
}

// PromoteToKeep packages a single Notebook entry into a Keep-writable
// [Proposal] for later Watchmaster approval. The helper is read-only on
// the Notebook DB — it loads the entry by id from `entry` and the
// embedding bytes from `entry_vec` in a single SELECT, deserialises the
// embedding back into `[]float32`, and stamps fresh provenance fields
// (UUID v7 [Proposal.ProposalID], default [ScopeOrg], current epoch ms
// [Proposal.ProposedAt]).
//
// On success returns the populated [*Proposal] and nil error. When a
// [Logger] has been wired via [WithLogger] the helper additionally emits
// a single `notebook_promotion_proposed` audit event AFTER the read
// completes (this is a read-only op, so the audit emit cannot rollback
// any DB state). The audit payload carries `agent_id`, `entry_id`,
// `proposal_id`, `category`, and `proposed_at` (RFC3339Nano UTC) — it
// excludes `content`, `embedding`, and `subject` to mirror the M2b.7
// PII / large-field discipline.
//
// # Errors
//
//   - [ErrInvalidEntry] when `entryID` is empty (or otherwise non-canonical
//     UUID); returned without touching the DB. No audit event emitted.
//   - [ErrNotFound] when `entryID` is well-formed but no row in `entry`
//     matches; the helper does NOT differentiate between "missing in
//     `entry`" and "missing in `entry_vec`" — both surface as ErrNotFound
//     because the M2b.1 sync contract guarantees they cannot diverge.
//     No audit event emitted.
//   - `fmt.Errorf("audit emit: %w", logErr)` when the read succeeded but
//     the [Logger.LogAppend] call failed. The returned [*Proposal] is
//     non-nil and fully populated — the partial-failure shape mirrors
//     [DB.Remember] / [DB.Forget] (M2b.7) so the caller can retry just
//     the audit emit with the same payload.
//
// Superseded entries (rows with non-NULL `superseded_by`) ARE promotable.
// PromoteToKeep does not filter on supersession: M6.2's Watchmaster-side
// approval flow may apply that policy on its end, but a caller that
// explicitly asks to promote a superseded entry receives a proposal for
// it. This decision keeps the helper's semantics narrow — "given an id,
// produce a proposal" — and avoids hiding rows the caller already named.
func (d *DB) PromoteToKeep(ctx context.Context, entryID string) (*Proposal, error) {
	if !uuidPattern.MatchString(entryID) {
		return nil, ErrInvalidEntry
	}

	var (
		category    string
		subject     sql.NullString
		content     string
		createdAt   int64
		toolVersion sql.NullString
		embedBlob   []byte
	)
	if err := d.sql.QueryRowContext(ctx, `
		SELECT e.category, e.subject, e.content, e.created_at, e.tool_version,
		       v.embedding
		FROM entry e
		JOIN entry_vec v ON v.id = e.id
		WHERE e.id = ?
	`, entryID).Scan(
		&category, &subject, &content, &createdAt, &toolVersion, &embedBlob,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("notebook: load entry for promote: %w", err)
	}

	embedding, err := deserializeEmbedding(embedBlob)
	if err != nil {
		return nil, fmt.Errorf("notebook: deserialize embedding: %w", err)
	}

	proposalID, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("notebook: generate proposal uuid v7: %w", err)
	}

	proposedAt := time.Now().UnixMilli()
	p := &Proposal{
		Subject:         stringFromNullable(subject),
		Content:         content,
		Embedding:       embedding,
		ToolVersion:     stringPtrFromNullable(toolVersion),
		ProposalID:      proposalID.String(),
		AgentID:         d.agentID,
		NotebookEntryID: entryID,
		Category:        category,
		Scope:           ScopeOrg,
		SourceCreatedAt: createdAt,
		ProposedAt:      proposedAt,
	}

	// Audit emit (M2b.7-style). Read is complete — the proposal struct
	// holds the only durable artifact this helper produces (no DB write
	// happens here; the future M6.2 path is the one that actually writes
	// into Keep). A LogAppend failure leaves the proposal valid — the
	// caller can retry just the audit with the same payload shape, or
	// proceed with the proposal regardless.
	//
	// Payload excludes `content`, `embedding`, `subject` by design (PII
	// / large fields). The audit log answers "promotion was proposed",
	// not "here is what was proposed".
	if d.logger != nil {
		payload, err := json.Marshal(map[string]any{
			"agent_id":    d.agentID,
			"entry_id":    entryID,
			"proposal_id": p.ProposalID,
			"category":    category,
			"proposed_at": time.Unix(0, proposedAt*int64(time.Millisecond)).UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			return p, fmt.Errorf("audit marshal: %w", err)
		}
		if _, err := d.logger.LogAppend(ctx, keepclient.LogAppendRequest{
			EventType: promoteEventType,
			Payload:   payload,
		}); err != nil {
			return p, fmt.Errorf("audit emit: %w", err)
		}
	}
	return p, nil
}

// deserializeEmbedding is the inverse of `sqlitevec.SerializeFloat32`: the
// upstream binding writes `binary.Write(buf, binary.LittleEndian, []float32)`
// (see asg017/sqlite-vec-go-bindings/cgo/lib.go), so the inverse is a
// `binary.Read` of the same length / byte order. The binding does not
// expose a Deserialize helper, so we mirror the canonical encoding
// inline. Length is always [EmbeddingDim] floats (= 4 * EmbeddingDim
// bytes); a mismatched blob length surfaces a clear error rather than
// silently truncating.
func deserializeEmbedding(blob []byte) ([]float32, error) {
	const floatBytes = 4
	want := EmbeddingDim * floatBytes
	if len(blob) != want {
		return nil, fmt.Errorf("blob length %d, want %d", len(blob), want)
	}
	out := make([]float32, EmbeddingDim)
	if err := binary.Read(bytes.NewReader(blob), binary.LittleEndian, out); err != nil {
		return nil, err
	}
	return out, nil
}
