// watchmaster_readonly.go registers the three M6.2.a read-only Watchmaster
// JSON-RPC methods on the Go-side [Host]:
//
//   - watchmaster.list_watchkeepers
//   - watchmaster.report_cost
//   - watchmaster.report_health
//
// All three handlers consume the same [WatchmasterReadDeps] bundle: a
// [spawn.WatchmasterReadClient] for the underlying keepclient calls plus
// a [ClaimResolver] that yields the [spawn.Claim] for a given context.
// The closure-capture pattern mirrors M5.5.d.a.b NewNotebookRememberHandler
// and M6.1.b NewSlackAppCreateHandler — captured at registration time, no
// package-level state.
//
// Wire protocol — list_watchkeepers:
//
//	request params: {status string (optional), limit number (optional)}
//	response:       {items [{id, manifest_id, lead_human_id, status, ...}]}
//
// Wire protocol — report_cost:
//
//	request params: {agent_id string (optional), event_type_prefix string (optional), limit number (optional)}
//	response:       {agent_id, event_type_prefix, prompt_tokens, completion_tokens, event_count, scanned_rows}
//
// Wire protocol — report_health:
//
//	request params: {agent_id string (optional)}
//	response:       {item {...}|null, count_pending, count_active, count_retired, count_total}
//
// Error mapping (all three handlers):
//   - [spawn.ErrInvalidClaim]   → -32602 (InvalidParams)
//   - [spawn.ErrInvalidRequest] → -32602 (InvalidParams)
//   - any other error           → -32603 (InternalError, wraps message)
//
// NONE of the three handlers map to -32005 (ToolUnauthorized) or -32007
// (ApprovalRequired); read-only tools do not consult the authority
// matrix and emit no audit chain. The harness ACL gate (M5.5.b.a) is
// what actually gates by manifest toolset — that runs TS-side BEFORE
// the wire call reaches this layer.
package harnessrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// WatchmasterReadDeps is the dependency bundle the three read-only
// handlers close over. Held in a struct rather than passed as separate
// parameters so the per-handler constructor signatures stay scannable
// (every handler takes one `deps` plus its method-specific args).
type WatchmasterReadDeps struct {
	// Client is the read-side keepclient surface the tools consume.
	// Required; passing nil panics from inside the constructor.
	Client spawn.WatchmasterReadClient

	// ResolveClaim is the closure the handler consults to derive the
	// [spawn.Claim] for a given context. Required; passing nil
	// panics from inside the constructor. M6.3 wiring supplies a
	// real resolver (reads the active manifest, projects
	// authority_matrix); tests inject a stub returning the canonical
	// Watchmaster claim.
	ResolveClaim ClaimResolver
}

// ── list_watchkeepers ────────────────────────────────────────────────────────

// listWatchkeepersParams is the wire shape decoded from the
// `watchmaster.list_watchkeepers` params field. Field names use
// snake_case to match the TS-side zod schema in
// `harness/src/builtinTools.ts`.
type listWatchkeepersParams struct {
	Status string `json:"status"`
	Limit  int    `json:"limit"`
}

// NewListWatchkeepersHandler returns a [MethodHandler] that implements
// `watchmaster.list_watchkeepers`. Closes over `deps`. Panics on a nil
// client / claim resolver — matches the panic-on-nil-dependency
// discipline of [spawn.NewSlackAppRPC].
func NewListWatchkeepersHandler(deps WatchmasterReadDeps) MethodHandler {
	if deps.Client == nil {
		panic("harnessrpc: NewListWatchkeepersHandler: client must not be nil")
	}
	if deps.ResolveClaim == nil {
		panic("harnessrpc: NewListWatchkeepersHandler: claim resolver must not be nil")
	}
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		// Empty params is allowed: list_watchkeepers has no required
		// fields. nil decodes as zero-valued [listWatchkeepersParams]
		// which forwards to "no filter, server default limit".
		var p listWatchkeepersParams
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, NewRPCError(ErrCodeInvalidParams,
					fmt.Sprintf("watchmaster.list_watchkeepers: decode params: %v", err))
			}
		}
		claim := deps.ResolveClaim(ctx)
		result, err := spawn.ListWatchkeepers(ctx, deps.Client, spawn.ListWatchkeepersRequest{
			Status: p.Status,
			Limit:  p.Limit,
		}, claim)
		if err != nil {
			return nil, mapWatchmasterReadError("watchmaster.list_watchkeepers", err)
		}
		return result, nil
	}
}

// ── report_cost ──────────────────────────────────────────────────────────────

// reportCostParams is the wire shape decoded from the
// `watchmaster.report_cost` params field.
type reportCostParams struct {
	AgentID         string `json:"agent_id"`
	EventTypePrefix string `json:"event_type_prefix"`
	Limit           int    `json:"limit"`
}

// NewReportCostHandler returns a [MethodHandler] that implements
// `watchmaster.report_cost`. Closes over `deps`.
func NewReportCostHandler(deps WatchmasterReadDeps) MethodHandler {
	if deps.Client == nil {
		panic("harnessrpc: NewReportCostHandler: client must not be nil")
	}
	if deps.ResolveClaim == nil {
		panic("harnessrpc: NewReportCostHandler: claim resolver must not be nil")
	}
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		var p reportCostParams
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, NewRPCError(ErrCodeInvalidParams,
					fmt.Sprintf("watchmaster.report_cost: decode params: %v", err))
			}
		}
		claim := deps.ResolveClaim(ctx)
		result, err := spawn.ReportCost(ctx, deps.Client, spawn.ReportCostRequest{
			AgentID:         p.AgentID,
			EventTypePrefix: p.EventTypePrefix,
			Limit:           p.Limit,
		}, claim)
		if err != nil {
			return nil, mapWatchmasterReadError("watchmaster.report_cost", err)
		}
		return result, nil
	}
}

// ── report_health ────────────────────────────────────────────────────────────

// reportHealthParams is the wire shape decoded from the
// `watchmaster.report_health` params field.
type reportHealthParams struct {
	AgentID string `json:"agent_id"`
}

// NewReportHealthHandler returns a [MethodHandler] that implements
// `watchmaster.report_health`. Closes over `deps`.
func NewReportHealthHandler(deps WatchmasterReadDeps) MethodHandler {
	if deps.Client == nil {
		panic("harnessrpc: NewReportHealthHandler: client must not be nil")
	}
	if deps.ResolveClaim == nil {
		panic("harnessrpc: NewReportHealthHandler: claim resolver must not be nil")
	}
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		var p reportHealthParams
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, NewRPCError(ErrCodeInvalidParams,
					fmt.Sprintf("watchmaster.report_health: decode params: %v", err))
			}
		}
		claim := deps.ResolveClaim(ctx)
		result, err := spawn.ReportHealth(ctx, deps.Client, spawn.ReportHealthRequest{
			AgentID: p.AgentID,
		}, claim)
		if err != nil {
			return nil, mapWatchmasterReadError("watchmaster.report_health", err)
		}
		return result, nil
	}
}

// mapWatchmasterReadError translates a [spawn] sentinel into the
// matching JSON-RPC error code. See the package docblock for the
// mapping rationale. Falls back to -32603 internal error wrapping the
// underlying error message verbatim. `methodName` is the wire method
// name the error message echoes for debuggability.
func mapWatchmasterReadError(methodName string, err error) error {
	switch {
	case errors.Is(err, spawn.ErrInvalidClaim):
		return NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("%s: %v", methodName, err))
	case errors.Is(err, spawn.ErrInvalidRequest):
		return NewRPCError(ErrCodeInvalidParams, fmt.Sprintf("%s: %v", methodName, err))
	default:
		return fmt.Errorf("%s: %w", methodName, err)
	}
}
