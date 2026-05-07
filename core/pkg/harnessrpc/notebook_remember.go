// notebook_remember.go registers the `notebook.remember` JSON-RPC method on the
// Go-side [Host]. This is the first real method on the bidirectional seam
// landed in M5.5.d.a.a (ROADMAP §M5.5.d.a.b).
//
// Handler: [NewNotebookRememberHandler]
//
// Wire protocol:
//
//	request params: {agentID string, category string, subject string, content string}
//	response:       {id string}
//
// Error codes:
//   - -32602 InvalidParams: missing agentID, content, or category.
//   - -32603 InternalError: agent not registered in supervisor, Embed failure,
//     or Remember failure.
//
// Embed input: "subject: content" when subject is non-empty; "content" otherwise.
// This keeps the vector representative of both fields without inflating the
// embedding for entries with a trivial subject.
package harnessrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// rememberParams is the wire shape decoded from the `notebook.remember` params
// field. All four fields are required; the handler validates them before any
// side-effectful work.
type rememberParams struct {
	AgentID  string `json:"agentID"`
	Category string `json:"category"`
	Subject  string `json:"subject"`
	Content  string `json:"content"`
}

// rememberResult is the response shape returned on success.
type rememberResult struct {
	ID string `json:"id"`
}

// NewNotebookRememberHandler returns a [MethodHandler] that implements
// `notebook.remember`. The returned handler closes over `supervisor` and
// `embedder` — no package-level state is used.
//
// Param validation order: agentID → content → category (so the first missing
// field surfaces a clear message). All three missing-field cases return
// ErrCodeInvalidParams (-32602). Downstream failures (Lookup miss, Embed error,
// Remember error) return ErrCodeInternalError (-32603).
func NewNotebookRememberHandler(supervisor *runtime.NotebookSupervisor, embedder llm.EmbeddingProvider) MethodHandler {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		// Decode params.
		if len(params) == 0 {
			return nil, NewRPCError(ErrCodeInvalidParams, "notebook.remember: params must not be null")
		}
		var p rememberParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("notebook.remember: decode params: %v", err))
		}

		// Validate required fields.
		if p.AgentID == "" {
			return nil, NewRPCError(ErrCodeInvalidParams, "notebook.remember: agentID is required")
		}
		if p.Content == "" {
			return nil, NewRPCError(ErrCodeInvalidParams, "notebook.remember: content is required")
		}
		if p.Category == "" {
			return nil, NewRPCError(ErrCodeInvalidParams, "notebook.remember: category is required")
		}

		// Lookup the per-agent notebook DB.
		db, ok := supervisor.Lookup(p.AgentID)
		if !ok {
			return nil, fmt.Errorf("internal error: agent not registered: %s", p.AgentID)
		}

		// Build the embed input: "subject: content" when subject non-empty.
		embedInput := p.Content
		if p.Subject != "" {
			embedInput = p.Subject + ": " + p.Content
		}

		// Compute the embedding.
		embedding, err := embedder.Embed(ctx, embedInput)
		if err != nil {
			return nil, fmt.Errorf("internal error: embed failed: %w", err)
		}

		// Build the entry. ID is pre-generated so the caller gets it back in
		// the response; Remember will preserve it (it only auto-generates when
		// Entry.ID is empty).
		entryID := uuid.NewString()
		entry := notebook.Entry{
			ID:        entryID,
			Category:  p.Category,
			Subject:   p.Subject,
			Content:   p.Content,
			CreatedAt: time.Now().UnixMilli(),
			Embedding: embedding,
		}

		// Persist.
		id, err := db.Remember(ctx, entry)
		if err != nil {
			return nil, fmt.Errorf("internal error: remember failed: %w", err)
		}

		return rememberResult{ID: id}, nil
	}
}
