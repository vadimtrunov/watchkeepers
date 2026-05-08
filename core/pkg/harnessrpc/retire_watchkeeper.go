// retire_watchkeeper.go registers the M6.2.c lead-approval-gated
// `watchmaster.retire_watchkeeper` JSON-RPC method on the Go-side
// [Host].
//
// Closes over a [WatchmasterWriteDeps] bundle (shared with the M6.2.b
// manifest-bump handlers): a [spawn.WatchmasterWriteClient] for the
// underlying keepclient surface, a [keepersLogAppender] for the audit
// chain, plus a [ClaimResolver] that yields the [spawn.Claim] for a
// given context. The closure-capture pattern mirrors M5.5.d.a.b
// NewNotebookRememberHandler / M6.1.b NewSlackAppCreateHandler /
// M6.2.a / M6.2.b — captured at registration time, no package-level
// state.
//
// Wire protocol — retire_watchkeeper:
//
//	request params: {
//	    agent_id        string,
//	    approval_token  string,
//	}
//	response:       {}    (empty success envelope — retire has no return value)
//
// Error mapping (mirrors the M6.2.b manifest-bump → JSON-RPC mapping):
//   - [spawn.ErrInvalidClaim]      → -32602 (InvalidParams)
//   - [spawn.ErrInvalidRequest]    → -32602 (InvalidParams)
//   - [spawn.ErrUnauthorized]      → -32005 (ToolUnauthorized)
//   - [spawn.ErrApprovalRequired]  → -32007 (ApprovalRequired)
//   - any other error              → -32603 (InternalError, wraps the
//     underlying message — typically a [keepclient] state-transition
//     rejection)
package harnessrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// retireWatchkeeperParams is the wire shape decoded from the
// `watchmaster.retire_watchkeeper` params field. Field names use
// snake_case to match the TS-side zod schema and the keepers_log
// payload keys.
type retireWatchkeeperParams struct {
	AgentID       string `json:"agent_id"`
	ApprovalToken string `json:"approval_token"`
}

// retireWatchkeeperResult is the response shape returned on success.
// The retire tool returns no fields — an empty struct round-trips as
// `{}` on the wire so a TS caller receives a typed success envelope
// rather than a raw `null`.
type retireWatchkeeperResult struct{}

// NewRetireWatchkeeperHandler returns a [MethodHandler] that
// implements `watchmaster.retire_watchkeeper`. Closes over `deps`.
// Panics on a nil dependency — matches the panic-on-nil-dependency
// discipline of the M6.2.b manifest-bump constructors.
func NewRetireWatchkeeperHandler(deps WatchmasterWriteDeps) MethodHandler {
	checkWriteDeps("NewRetireWatchkeeperHandler", deps)
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		if len(params) == 0 {
			return nil, NewRPCError(ErrCodeInvalidParams,
				"watchmaster.retire_watchkeeper: params must not be null")
		}
		var p retireWatchkeeperParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(ErrCodeInvalidParams,
				fmt.Sprintf("watchmaster.retire_watchkeeper: decode params: %v", err))
		}
		claim := deps.ResolveClaim(ctx)
		if err := spawn.RetireWatchkeeper(ctx, deps.Client, deps.Logger, spawn.RetireWatchkeeperRequest{
			AgentID:       p.AgentID,
			ApprovalToken: p.ApprovalToken,
		}, claim); err != nil {
			return nil, mapRetireError(err)
		}
		return retireWatchkeeperResult{}, nil
	}
}

// mapRetireError translates a [spawn] sentinel into the matching
// JSON-RPC error code. Mirrors [mapManifestBumpError] verbatim except
// for the methodName prefix; kept as a separate function rather than
// reusing the manifest-bump mapper so a future divergence (e.g. a
// retire-specific keep-server sentinel) is a one-line change here.
func mapRetireError(err error) error {
	const methodName = "watchmaster.retire_watchkeeper"
	switch {
	case errors.Is(err, spawn.ErrInvalidClaim):
		return NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("%s: %v", methodName, err))
	case errors.Is(err, spawn.ErrInvalidRequest):
		return NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("%s: %v", methodName, err))
	case errors.Is(err, spawn.ErrUnauthorized):
		return NewRPCError(ErrCodeToolUnauthorized, fmt.Sprintf("%s: %v", methodName, err))
	case errors.Is(err, spawn.ErrApprovalRequired):
		return NewRPCError(ErrCodeApprovalRequired, fmt.Sprintf("%s: %v", methodName, err))
	default:
		return fmt.Errorf("%s: %w", methodName, err)
	}
}
