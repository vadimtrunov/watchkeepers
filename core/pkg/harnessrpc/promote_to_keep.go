// promote_to_keep.go registers the M6.2.d lead-approval-gated
// `watchmaster.promote_to_keep` JSON-RPC method on the Go-side [Host].
//
// Closes over a [WatchmasterPromoteDeps] bundle: a
// [spawn.WatchmasterWriteClient] for the keep-write seam, a
// [spawn.WatchmasterNotebookClient] for the notebook-read seam, a
// [keepersLogAppender] for the audit chain, plus a [ClaimResolver]
// that yields the [spawn.Claim] for a given context. The closure-
// capture pattern mirrors M5.5.d.a.b NewNotebookRememberHandler /
// M6.1.b NewSlackAppCreateHandler / M6.2.a / M6.2.b / M6.2.c —
// captured at registration time, no package-level state.
//
// Wire protocol — promote_to_keep:
//
//	request params: {
//	    agent_id           string,
//	    notebook_entry_id  string,
//	    approval_token     string,
//	}
//	response:       {chunk_id string, proposal_id string, notebook_entry_id string}
//
// Error mapping (mirrors the M6.1.b / M6.2.b/c spawn → JSON-RPC mapping):
//   - [spawn.ErrInvalidClaim]      → -32602 (InvalidParams)
//   - [spawn.ErrInvalidRequest]    → -32602 (InvalidParams)
//   - [spawn.ErrUnauthorized]      → -32005 (ToolUnauthorized)
//   - [spawn.ErrApprovalRequired]  → -32007 (ApprovalRequired)
//   - [notebook.ErrNotFound]       → -32011 (ToolNotFound — application-
//     range; lets a TS caller branch on "notebook entry missing"
//     without string-matching the message)
//   - [notebook.ErrInvalidEntry]   → -32602 (InvalidParams — non-canonical
//     UUID is a wire-shape bug)
//   - any other error              → -32603 (InternalError, wraps the
//     underlying message — typically a [keepclient] write rejection)
package harnessrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ErrCodeToolNotFound is the application-range JSON-RPC code the
// promote_to_keep handler emits when the underlying notebook read
// surfaces [notebook.ErrNotFound] (well-formed entry id with no row
// behind it). Lives in the application slice (-32099..-32000) so it
// never collides with the JSON-RPC reserved band; a TS caller branches
// on this code to surface a "no such notebook entry" affordance
// without string-matching the message. The numeric value mirrors the
// TS-side {@link ToolErrorCode.ToolUnknown} family — chosen so the
// future M6.3 wire vocabulary stays consistent without a renumber.
const ErrCodeToolNotFound = -32011

// WatchmasterPromoteDeps is the dependency bundle the promote_to_keep
// handler closes over. Held in a struct rather than passed as separate
// parameters so the constructor signature stays scannable. Distinct
// from [WatchmasterWriteDeps] because promote_to_keep needs the
// notebook-read seam too — a future tool that does not need the
// notebook-side seam should keep using [WatchmasterWriteDeps].
type WatchmasterPromoteDeps struct {
	// Client is the keep-write surface (insert into knowledge_chunk
	// via [keepclient.Client.Store]). Required; passing nil panics
	// from inside the constructor.
	Client spawn.WatchmasterWriteClient

	// NotebookClient is the notebook-read surface
	// ([notebook.DB.PromoteToKeep]). Required; passing nil panics
	// from inside the constructor.
	NotebookClient spawn.WatchmasterNotebookClient

	// Logger is the audit-chain seam the tool emits its 3-event
	// chain through (requested → notebook_promoted_to_keep →
	// succeeded, or requested + failed). Required; passing nil
	// panics from inside the constructor.
	Logger keepersLogAppender

	// ResolveClaim is the closure the handler consults to derive
	// the [spawn.Claim] for a given context. Required; passing
	// nil panics from inside the constructor.
	ResolveClaim ClaimResolver
}

// promoteToKeepParams is the wire shape decoded from the
// `watchmaster.promote_to_keep` params field. Field names use
// snake_case to match the TS-side zod schema and the keepers_log
// payload keys.
type promoteToKeepParams struct {
	AgentID         string `json:"agent_id"`
	NotebookEntryID string `json:"notebook_entry_id"`
	ApprovalToken   string `json:"approval_token"`
}

// promoteToKeepResult is the response shape returned on success.
// Carries the keep-server-assigned chunk id plus the notebook-side
// proposal id and source entry id verbatim from
// [spawn.PromoteToKeepResult] so the TS caller can correlate the
// audit chain without a re-read.
type promoteToKeepResult struct {
	ChunkID         string `json:"chunk_id"`
	ProposalID      string `json:"proposal_id"`
	NotebookEntryID string `json:"notebook_entry_id"`
}

// NewPromoteToKeepHandler returns a [MethodHandler] that implements
// `watchmaster.promote_to_keep`. Closes over `deps`. Panics on a nil
// dependency — matches the panic-on-nil-dependency discipline of the
// M6.2.b/c constructors.
func NewPromoteToKeepHandler(deps WatchmasterPromoteDeps) MethodHandler {
	checkPromoteDeps("NewPromoteToKeepHandler", deps)
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		if len(params) == 0 {
			return nil, NewRPCError(ErrCodeInvalidParams,
				"watchmaster.promote_to_keep: params must not be null")
		}
		var p promoteToKeepParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(ErrCodeInvalidParams,
				fmt.Sprintf("watchmaster.promote_to_keep: decode params: %v", err))
		}
		claim := deps.ResolveClaim(ctx)
		result, err := spawn.PromoteToKeep(ctx, deps.Client, deps.NotebookClient, deps.Logger,
			spawn.PromoteToKeepRequest{
				AgentID:         p.AgentID,
				NotebookEntryID: p.NotebookEntryID,
				ApprovalToken:   p.ApprovalToken,
			}, claim)
		if err != nil {
			return nil, mapPromoteError(err)
		}
		return promoteToKeepResult{
			ChunkID:         result.ChunkID,
			ProposalID:      result.ProposalID,
			NotebookEntryID: result.NotebookEntryID,
		}, nil
	}
}

// checkPromoteDeps centralises the panic-on-nil-dependency guards the
// promote constructor enforces. The constructor name is embedded in
// the panic message so a boot-time mis-wiring surfaces a scannable
// stack trace.
func checkPromoteDeps(ctorName string, deps WatchmasterPromoteDeps) {
	if deps.Client == nil {
		panic(fmt.Sprintf("harnessrpc: %s: client must not be nil", ctorName))
	}
	if deps.NotebookClient == nil {
		panic(fmt.Sprintf("harnessrpc: %s: notebook client must not be nil", ctorName))
	}
	if deps.Logger == nil {
		panic(fmt.Sprintf("harnessrpc: %s: logger must not be nil", ctorName))
	}
	if deps.ResolveClaim == nil {
		panic(fmt.Sprintf("harnessrpc: %s: claim resolver must not be nil", ctorName))
	}
}

// mapPromoteError translates a [spawn] / [notebook] sentinel into the
// matching JSON-RPC error code. Mirrors [mapManifestBumpError] /
// [mapRetireError] for the spawn sentinels, and adds the
// [notebook.ErrNotFound] → -32011 mapping documented in the package
// docblock.
func mapPromoteError(err error) error {
	const methodName = "watchmaster.promote_to_keep"
	switch {
	case errors.Is(err, spawn.ErrInvalidClaim):
		return NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("%s: %v", methodName, err))
	case errors.Is(err, spawn.ErrInvalidRequest):
		return NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("%s: %v", methodName, err))
	case errors.Is(err, spawn.ErrUnauthorized):
		return NewRPCError(ErrCodeToolUnauthorized, fmt.Sprintf("%s: %v", methodName, err))
	case errors.Is(err, spawn.ErrApprovalRequired):
		return NewRPCError(ErrCodeApprovalRequired, fmt.Sprintf("%s: %v", methodName, err))
	case errors.Is(err, notebook.ErrNotFound):
		return NewRPCError(ErrCodeToolNotFound, fmt.Sprintf("%s: %v", methodName, err))
	case errors.Is(err, notebook.ErrInvalidEntry):
		return NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("%s: %v", methodName, err))
	default:
		return fmt.Errorf("%s: %w", methodName, err)
	}
}
