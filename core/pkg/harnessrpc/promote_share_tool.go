// promote_share_tool.go registers the M9.6 capability-gated
// `watchmaster.promote_share_tool` JSON-RPC method on the
// Go-side [Host]. The TS-side `promote_share_tool` builtin tool
// dispatches here through the bidirectional [RpcClient] in
// `harness/src/jsonrpc.ts`.
//
// Handler: [NewPromoteShareToolHandler]
//
// Wire protocol:
//
//	request params: {
//	    proposer_id      string,
//	    source_name      string,
//	    tool_name        string,
//	    target_hint      string ("platform" | "private"),
//	    reason           string,
//	    capability_token string,
//	}
//	response:       {pr_number int, pr_html_url string, branch_name string,
//	                tool_version string, correlation_id string,
//	                lead_notified bool}
//
// Capability gating: the handler validates `capability_token`
// against the `tool:share` scope via [capability.Broker.Validate]
// BEFORE invoking the orchestrator. `tool:share` is OFF by default
// and granted per-Watchkeeper by the lead via the M9.4-style
// approval card (or by direct operator-issued capability via
// [capability.Broker.Issue]).
//
// Capability failure mapping (only the sentinels [capability.Broker.Validate]
// actually returns; iter-1 M8 fix dropped three previously-listed
// unreachable cases — see [capability.Broker.Validate]:241-291):
//
//   - [capability.ErrInvalidToken]   → -32005 (ToolUnauthorized)
//   - [capability.ErrTokenExpired]   → -32005 (ToolUnauthorized)
//   - [capability.ErrScopeMismatch]  → -32005 (ToolUnauthorized)
//   - [capability.ErrClosed]         → -32603 (InternalError)
//
// Share-error mapping (iter-1 M5/M9 fix: dropped misleading
// `ErrIdentityResolution → ToolUnauthorized`, routed
// `ErrSourceLookupMismatch` to InternalError as it's a programmer/
// wiring error, added [github] sentinel surfaces, and documented
// [toolshare.ErrManifestRead]):
//
//   - [toolshare.ErrInvalidShareRequest]   → -32602 (InvalidParams)
//   - [toolshare.ErrInvalidProposerID]     → -32602 (InvalidParams)
//   - [toolshare.ErrInvalidTarget]         → -32602 (InvalidParams)
//   - [toolshare.ErrUnknownSource]         → -32602 (InvalidParams)
//   - [toolshare.ErrToolMissing]           → -32602 (InvalidParams)
//   - [toolshare.ErrManifestRead]          → -32602 (InvalidParams)
//   - [github.ErrInvalidAuth]              → -32005 (ToolUnauthorized,
//     surfaces a "PAT expired / revoked" condition the agent caller
//     wants to discriminate)
//   - any other error              → -32603 (InternalError, wraps message)
//
// The handler does NOT include a `spawn.Claim`-style resolver
// because the share orchestrator's authorisation surface is the
// `tool:share` capability token alone — no manifest-based
// authority check, no spawn-side reapproval. The orchestrator
// already validates the proposer id post-resolver via
// [toolshare.ValidateProposerID].
package harnessrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/capability"
	"github.com/vadimtrunov/watchkeepers/core/pkg/github"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

// MaxCapabilityTokenLength bounds the agent-supplied
// `capability_token` length. The [capability.Broker]-issued tokens
// are 40-byte-prefixed-random-32-bytes URL-safe-base64 (~50 chars
// in practice); 512 covers any reasonable future encoding without
// passing unbounded garbage through to the broker's hot path.
// Iter-1 n8 fix (reviewer A).
const MaxCapabilityTokenLength = 512

// ShareCapabilityScope is the [capability.Broker] scope name the
// promote_share_tool handler validates the request token against.
// Mirror M9.6 roadmap text: "capability `tool:share` off by default
// and granted per-Watchkeeper by the lead".
const ShareCapabilityScope = "tool:share"

// promoteShareToolParams is the wire shape decoded from the
// `watchmaster.promote_share_tool` params field. Field names use
// snake_case to match the TS-side zod schema and the keepers_log
// payload keys.
type promoteShareToolParams struct {
	ProposerID      string `json:"proposer_id"`
	SourceName      string `json:"source_name"`
	ToolName        string `json:"tool_name"`
	TargetHint      string `json:"target_hint"`
	Reason          string `json:"reason"`
	CapabilityToken string `json:"capability_token"`
}

// promoteShareToolResult is the response shape returned on success.
type promoteShareToolResult struct {
	PRNumber      int    `json:"pr_number"`
	PRHTMLURL     string `json:"pr_html_url"`
	BranchName    string `json:"branch_name"`
	ToolVersion   string `json:"tool_version"`
	CorrelationID string `json:"correlation_id"`
	LeadNotified  bool   `json:"lead_notified"`
}

// ShareCapabilityValidator is the subset of
// [*capability.Broker] [NewPromoteShareToolHandler] consumes.
// Defined locally so tests substitute a hand-rolled fake.
type ShareCapabilityValidator interface {
	Validate(ctx context.Context, token, scope string) error
}

// ShareOrchestrator is the subset of [*toolshare.Sharer]
// [NewPromoteShareToolHandler] consumes. Defined locally so tests
// substitute a hand-rolled fake and production wiring forwards to
// a real Sharer.
type ShareOrchestrator interface {
	Share(ctx context.Context, req toolshare.ShareRequest) (toolshare.ShareResult, error)
}

// PromoteShareToolDeps is the dependency bundle the
// promote_share_tool handler closes over.
type PromoteShareToolDeps struct {
	// Capability validates the `tool:share` scope token. Required;
	// passing nil panics from inside the constructor.
	Capability ShareCapabilityValidator

	// Sharer is the share orchestrator. Required; passing nil
	// panics from inside the constructor.
	Sharer ShareOrchestrator
}

// NewPromoteShareToolHandler returns a [MethodHandler] that
// implements `watchmaster.promote_share_tool`. Closes over `deps`.
// Panics on a nil dependency.
func NewPromoteShareToolHandler(deps PromoteShareToolDeps) MethodHandler {
	if deps.Capability == nil {
		panic("harnessrpc: NewPromoteShareToolHandler: capability must not be nil")
	}
	if deps.Sharer == nil {
		panic("harnessrpc: NewPromoteShareToolHandler: sharer must not be nil")
	}
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		if len(params) == 0 {
			return nil, NewRPCError(ErrCodeInvalidParams,
				"watchmaster.promote_share_tool: params must not be null")
		}
		var p promoteShareToolParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(ErrCodeInvalidParams,
				fmt.Sprintf("watchmaster.promote_share_tool: decode params: %v", err))
		}
		if p.CapabilityToken == "" {
			return nil, NewRPCError(ErrCodeToolUnauthorized,
				"watchmaster.promote_share_tool: capability_token is required")
		}
		if len(p.CapabilityToken) > MaxCapabilityTokenLength {
			return nil, NewRPCError(ErrCodeInvalidParams,
				fmt.Sprintf("watchmaster.promote_share_tool: capability_token %d bytes (max %d)",
					len(p.CapabilityToken), MaxCapabilityTokenLength))
		}
		if err := deps.Capability.Validate(ctx, p.CapabilityToken, ShareCapabilityScope); err != nil {
			return nil, mapShareCapabilityError(err)
		}
		result, err := deps.Sharer.Share(ctx, toolshare.ShareRequest{
			SourceName:     p.SourceName,
			ToolName:       p.ToolName,
			TargetHint:     toolshare.TargetSource(p.TargetHint),
			Reason:         p.Reason,
			ProposerIDHint: p.ProposerID,
		})
		if err != nil {
			return nil, mapShareError(err)
		}
		return promoteShareToolResult{
			PRNumber:      result.PRNumber,
			PRHTMLURL:     result.PRHTMLURL,
			BranchName:    result.BranchName,
			ToolVersion:   result.ToolVersion,
			CorrelationID: result.CorrelationID,
			LeadNotified:  result.LeadNotified,
		}, nil
	}
}

// mapShareCapabilityError translates a [capability] sentinel into
// the matching JSON-RPC error code.
//
// Iter-1 M8 fix (reviewer B): `capability.Broker.Validate` only
// returns [ErrInvalidToken], [ErrTokenExpired], [ErrScopeMismatch],
// [ErrClosed], or `ctx.Err()`. [ErrInvalidScope] /
// [ErrInvalidOrganization] / [ErrOrganizationMismatch] are
// unreachable via this entry point — they only surface from
// `Issue` / `ValidateForOrg`. Dropping the unreachable cases keeps
// the doc-claim ⇔ implementation parity that the M9.4.d
// contract-test discipline demands of every reviewer-vocabulary
// surface.
func mapShareCapabilityError(err error) error {
	const methodName = "watchmaster.promote_share_tool"
	switch {
	case errors.Is(err, capability.ErrInvalidToken),
		errors.Is(err, capability.ErrTokenExpired),
		errors.Is(err, capability.ErrScopeMismatch):
		return NewRPCError(ErrCodeToolUnauthorized, fmt.Sprintf("%s: %v", methodName, err))
	default:
		return fmt.Errorf("%s: %w", methodName, err)
	}
}

// mapShareError translates a [toolshare] or [github] sentinel into
// the matching JSON-RPC error code.
//
// Iter-1 fixes:
//   - M5 (reviewer A): `ErrIdentityResolution` was mapped to
//     `ToolUnauthorized` — semantically wrong (resolver outage is
//     an internal error, not "the caller's capability didn't grant
//     this scope"). Dropped.
//   - M9 (reviewer B): `ErrSourceLookupMismatch` was mapped to
//     `InvalidParams` — wrong, that's a programmer / wiring error
//     (the resolver returned a record with a name field that
//     disagrees with the requested name). Dropped from the
//     InvalidParams set; falls through to InternalError.
//   - n5 (reviewer A) + n6: added [github.ErrInvalidAuth] →
//     ToolUnauthorized and merged the two prior duplicate
//     InvalidParams case-blocks into one.
func mapShareError(err error) error {
	const methodName = "watchmaster.promote_share_tool"
	switch {
	case errors.Is(err, toolshare.ErrInvalidShareRequest),
		errors.Is(err, toolshare.ErrInvalidProposerID),
		errors.Is(err, toolshare.ErrInvalidTarget),
		errors.Is(err, toolshare.ErrUnknownSource),
		errors.Is(err, toolshare.ErrToolMissing),
		errors.Is(err, toolshare.ErrManifestRead):
		return NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("%s: %v", methodName, err))
	case errors.Is(err, toolshare.ErrEmptyResolvedIdentity):
		return NewRPCError(ErrCodeToolUnauthorized, fmt.Sprintf("%s: %v", methodName, err))
	case errors.Is(err, github.ErrInvalidAuth):
		return NewRPCError(ErrCodeToolUnauthorized, fmt.Sprintf("%s: %v", methodName, err))
	default:
		return fmt.Errorf("%s: %w", methodName, err)
	}
}
