// slack_app_create.go registers the `slack.app_create` JSON-RPC method on the
// Go-side [Host]. M6.1.b — the first privileged-RPC method on the bidirectional
// seam. The TS-side `slack_app_create` builtin tool dispatches here through
// the bidirectional [RpcClient] in `harness/src/jsonrpc.ts`.
//
// Handler: [NewSlackAppCreateHandler]
//
// Wire protocol:
//
//	request params: {
//	    agent_id        string,
//	    app_name        string,
//	    app_description string (optional),
//	    scopes          []string (optional),
//	    approval_token  string,
//	}
//	response:       {app_id string}
//
// Error mapping (Go-side spawn sentinel → JSON-RPC code):
//   - [spawn.ErrInvalidClaim]      → -32602 (invalid params)
//   - [spawn.ErrInvalidRequest]    → -32602 (invalid params)
//   - [spawn.ErrUnauthorized]      → -32005 (ToolUnauthorized — matches the
//     M5.5.b.a TS-side ACL gate code so wire callers see one code per
//     "you cannot do this" failure mode)
//   - [spawn.ErrApprovalRequired]  → -32007 (application-range — empty
//     approval_token is a runtime workflow gate, not a wire-shape error)
//   - any other error              → -32603 (internal error, wraps message)
//
// The handler resolves the [spawn.Claim] via the supplied closure; the
// closure shape mirrors M5.5.d.a.b's NewNotebookRememberHandler — captured
// at registration time, no package-level state. M6.2 (Watchmaster wiring)
// supplies a real claim resolver that reads the active manifest; M6.1.b
// tests inject a stub returning the canonical Watchmaster claim.
package harnessrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ErrCodeToolUnauthorized mirrors the TS-side
// `ToolErrorCode.ToolUnauthorized` (-32005, see
// `harness/src/invokeTool.ts`). The Go handler maps
// [spawn.ErrUnauthorized] to this code so a wire caller sees one
// "you cannot do this" code regardless of which side gated it.
const ErrCodeToolUnauthorized = -32005

// ErrCodeApprovalRequired is the application-range JSON-RPC code the
// handler emits when the privileged RPC rejected the call with
// [spawn.ErrApprovalRequired]. Lives in the application slice
// (-32099..-32000) so it never collides with the JSON-RPC reserved
// band; a TS caller branches on this code to surface a "lead approval
// pending" affordance without string-matching the message.
const ErrCodeApprovalRequired = -32007

// slackAppCreateParams is the wire shape decoded from the
// `slack.app_create` params field. Field names use snake_case to match
// the TS-side zod schema in `harness/src/builtinTools.ts` and the
// keepers_log payload keys.
type slackAppCreateParams struct {
	AgentID        string   `json:"agent_id"`
	AppName        string   `json:"app_name"`
	AppDescription string   `json:"app_description"`
	Scopes         []string `json:"scopes"`
	ApprovalToken  string   `json:"approval_token"`
}

// slackAppCreateResult is the response shape returned on success.
// Carries the platform-assigned app id verbatim from
// [spawn.CreateAppResult.AppID].
type slackAppCreateResult struct {
	AppID string `json:"app_id"`
}

// ClaimResolver is the closure shape the handler consults to derive
// the [spawn.Claim] for a given context. M6.2 wiring supplies a real
// resolver (reads the active manifest, projects authority_matrix);
// M6.1.b tests inject a stub returning the canonical Watchmaster
// claim. Defined as a named type rather than a bare `func` so the
// handler signature stays scannable.
type ClaimResolver func(ctx context.Context) spawn.Claim

// NewSlackAppCreateHandler returns a [MethodHandler] that implements
// `slack.app_create`. The returned handler closes over `rpc` and
// `claimResolver` — no package-level state is used, mirroring the
// M5.5.d.a.b NewNotebookRememberHandler pattern.
//
// Resolution order:
//  1. Decode params (-32602 on missing / malformed).
//  2. Resolve the [spawn.Claim] via `claimResolver(ctx)`.
//  3. Call `rpc.CreateApp(ctx, req, claim)`.
//  4. Map the spawn-package error to the matching JSON-RPC code (see
//     package docblock).
//  5. Return `{app_id}` on success.
func NewSlackAppCreateHandler(rpc spawn.SlackAppRPC, claimResolver ClaimResolver) MethodHandler {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		if len(params) == 0 {
			return nil, NewRPCError(ErrCodeInvalidParams, "slack.app_create: params must not be null")
		}
		var p slackAppCreateParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("slack.app_create: decode params: %v", err))
		}

		req := spawn.CreateAppRequest{
			AgentID:        p.AgentID,
			AppName:        p.AppName,
			AppDescription: p.AppDescription,
			Scopes:         p.Scopes,
			ApprovalToken:  p.ApprovalToken,
		}
		claim := claimResolver(ctx)

		result, err := rpc.CreateApp(ctx, req, claim)
		if err != nil {
			return nil, mapSpawnError(err)
		}

		return slackAppCreateResult{AppID: string(result.AppID)}, nil
	}
}

// mapSpawnError translates a [spawn] sentinel into the matching
// JSON-RPC error code. See the package docblock for the mapping
// rationale. Falls back to -32603 internal error wrapping the
// underlying error message verbatim.
func mapSpawnError(err error) error {
	switch {
	case errors.Is(err, spawn.ErrInvalidClaim):
		return NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("slack.app_create: %v", err))
	case errors.Is(err, spawn.ErrInvalidRequest):
		return NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("slack.app_create: %v", err))
	case errors.Is(err, spawn.ErrUnauthorized):
		return NewRPCError(ErrCodeToolUnauthorized, fmt.Sprintf("slack.app_create: %v", err))
	case errors.Is(err, spawn.ErrApprovalRequired):
		return NewRPCError(ErrCodeApprovalRequired, fmt.Sprintf("slack.app_create: %v", err))
	default:
		return fmt.Errorf("slack.app_create: %w", err)
	}
}
