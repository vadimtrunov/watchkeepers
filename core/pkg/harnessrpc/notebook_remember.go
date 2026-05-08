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

	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// validCategories is the closed set of allowed notebook entry categories,
// duplicated here from notebook.categoryEnum (which is unexported) so the
// handler can return -32602 InvalidParams before wasting an Embed call on
// an entry that would fail notebook.validate anyway.
// Source of truth: core/pkg/notebook/entry.go categoryEnum.
var validCategories = map[string]struct{}{
	notebook.CategoryLesson:           {},
	notebook.CategoryPreference:       {},
	notebook.CategoryObservation:      {},
	notebook.CategoryPendingTask:      {},
	notebook.CategoryRelationshipNote: {},
}

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
		// Validate category against the closed enum before any side-effectful
		// work. Unknown but non-empty category would fail inside
		// notebook.Remember as -32603 internal error instead of the
		// semantically-correct -32602 invalid params.
		// Source of truth: core/pkg/notebook/entry.go categoryEnum.
		if _, ok := validCategories[p.Category]; !ok {
			return nil, NewRPCError(ErrCodeInvalidParams,
				fmt.Sprintf("notebook.remember: category must be one of: lesson, preference, observation, pending_task, relationship_note; got %q", p.Category))
		}

		// Lookup the per-agent notebook DB.
		db, ok := supervisor.Lookup(p.AgentID)
		if !ok {
			return nil, NewRPCErrorData(ErrCodeInternalError,
				fmt.Sprintf("notebook.remember: agent not registered: %s", p.AgentID),
				map[string]any{"kind": "agent_not_registered"})
		}

		// Build the embed input: "subject: content" when subject non-empty.
		embedInput := p.Content
		if p.Subject != "" {
			embedInput = p.Subject + ": " + p.Content
		}

		// Compute the embedding.
		embedding, err := embedder.Embed(ctx, embedInput)
		if err != nil {
			return nil, NewRPCErrorData(ErrCodeInternalError,
				fmt.Sprintf("notebook.remember: embed failed: %v", err),
				map[string]any{"kind": "embed_failed"})
		}

		// Build the entry. Leave ID empty so notebook.Remember auto-generates
		// a UUID v7 (time-ordered). Pre-generating a v4 here would mix id
		// formats and defeat v7's sort-by-id property.
		entry := notebook.Entry{
			Category:  p.Category,
			Subject:   p.Subject,
			Content:   p.Content,
			CreatedAt: time.Now().UnixMilli(),
			Embedding: embedding,
		}

		// Persist and get the auto-generated v7 id back from Remember.
		id, err := db.Remember(ctx, entry)
		if err != nil {
			return nil, NewRPCErrorData(ErrCodeInternalError,
				fmt.Sprintf("notebook.remember: remember failed: %v", err),
				map[string]any{"kind": "remember_failed"})
		}

		return rememberResult{ID: id}, nil
	}
}
