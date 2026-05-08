// watchmaster_writes.go registers the three M6.2.b lead-approval-gated
// Watchmaster manifest-bump JSON-RPC methods on the Go-side [Host]:
//
//   - watchmaster.propose_spawn
//   - watchmaster.adjust_personality
//   - watchmaster.adjust_language
//
// Each handler closes over a [WatchmasterWriteDeps] bundle: a
// [spawn.WatchmasterWriteClient] for the underlying keepclient surface,
// a [keepersLogAppender] for the audit chain, plus a [ClaimResolver]
// that yields the [spawn.Claim] for a given context. The closure-
// capture pattern mirrors M5.5.d.a.b NewNotebookRememberHandler /
// M6.1.b NewSlackAppCreateHandler / M6.2.a NewListWatchkeepersHandler —
// captured at registration time, no package-level state.
//
// Wire protocol — propose_spawn:
//
//	request params: {
//	    agent_id        string,
//	    system_prompt   string,
//	    personality     string (optional),
//	    language        string (optional),
//	    approval_token  string,
//	}
//	response:       {manifest_version_id string, version_no number}
//
// Wire protocol — adjust_personality:
//
//	request params: {
//	    agent_id        string,
//	    new_personality string,
//	    approval_token  string,
//	}
//	response:       {manifest_version_id string, version_no number}
//
// Wire protocol — adjust_language:
//
//	request params: {
//	    agent_id        string,
//	    new_language    string,
//	    approval_token  string,
//	}
//	response:       {manifest_version_id string, version_no number}
//
// Error mapping (mirrors the M6.1.b spawn → JSON-RPC mapping):
//   - [spawn.ErrInvalidClaim]      → -32602 (InvalidParams)
//   - [spawn.ErrInvalidRequest]    → -32602 (InvalidParams)
//   - [spawn.ErrUnauthorized]      → -32005 (ToolUnauthorized — same
//     code the TS-side ACL gate emits, so a wire caller sees one
//     code per "you cannot do this" failure mode)
//   - [spawn.ErrApprovalRequired]  → -32007 (ApprovalRequired —
//     application-range; lets a TS caller branch on
//     "lead approval pending" without string-matching)
//   - any other error              → -32603 (InternalError, wraps
//     the underlying message)
package harnessrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// keepersLogAppender is the minimal subset of [keeperslog.Writer] the
// manifest-bump handlers consume. Defined locally at the seam so tests
// can substitute a tiny fake without standing up a real writer over a
// recording fake LocalKeepClient. Mirrors the seam pattern used in the
// spawn package.
type keepersLogAppender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// Compile-time assertion: the production [*keeperslog.Writer]
// satisfies [keepersLogAppender]. Pins the integration shape.
var _ keepersLogAppender = (*keeperslog.Writer)(nil)

// WatchmasterWriteDeps is the dependency bundle the three manifest-
// bump handlers close over. Held in a struct rather than passed as
// separate parameters so the per-handler constructor signatures stay
// scannable (every handler takes one `deps`).
type WatchmasterWriteDeps struct {
	// Client is the write-side keepclient surface the tools consume.
	// Required; passing nil panics from inside the constructor.
	Client spawn.WatchmasterWriteClient

	// Logger is the audit-chain seam the tools emit `requested` /
	// `succeeded` / `failed` rows through. Required; passing nil
	// panics from inside the constructor.
	Logger keepersLogAppender

	// ResolveClaim is the closure the handler consults to derive the
	// [spawn.Claim] for a given context. Required; passing nil
	// panics from inside the constructor. M6.3 wiring supplies a
	// real resolver (reads the active manifest, projects
	// authority_matrix); tests inject a stub returning a canonical
	// Watchmaster claim.
	ResolveClaim ClaimResolver
}

// ── propose_spawn ────────────────────────────────────────────────────────────

// proposeSpawnParams is the wire shape decoded from the
// `watchmaster.propose_spawn` params field. Field names use snake_case
// to match the TS-side zod schema and the keepers_log payload keys.
type proposeSpawnParams struct {
	AgentID       string `json:"agent_id"`
	SystemPrompt  string `json:"system_prompt"`
	Personality   string `json:"personality"`
	Language      string `json:"language"`
	ApprovalToken string `json:"approval_token"`
}

// manifestBumpResult is the response shape returned on success for all
// three manifest-bump handlers. Carries the platform-assigned
// manifest_version row UUID + the bumped version number verbatim from
// [spawn.ManifestBumpResult].
type manifestBumpResult struct {
	ManifestVersionID string `json:"manifest_version_id"`
	VersionNo         int    `json:"version_no"`
}

// NewProposeSpawnHandler returns a [MethodHandler] that implements
// `watchmaster.propose_spawn`. Closes over `deps`. Panics on a nil
// dependency — matches the panic-on-nil-dependency discipline of
// [spawn.NewSlackAppRPC] and the M6.2.a read-only handlers.
func NewProposeSpawnHandler(deps WatchmasterWriteDeps) MethodHandler {
	checkWriteDeps("NewProposeSpawnHandler", deps)
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		if len(params) == 0 {
			return nil, NewRPCError(ErrCodeInvalidParams, "watchmaster.propose_spawn: params must not be null")
		}
		var p proposeSpawnParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(ErrCodeInvalidParams,
				fmt.Sprintf("watchmaster.propose_spawn: decode params: %v", err))
		}
		claim := deps.ResolveClaim(ctx)
		result, err := spawn.ProposeSpawn(ctx, deps.Client, deps.Logger, spawn.ProposeSpawnRequest{
			AgentID:       p.AgentID,
			SystemPrompt:  p.SystemPrompt,
			Personality:   p.Personality,
			Language:      p.Language,
			ApprovalToken: p.ApprovalToken,
		}, claim)
		if err != nil {
			return nil, mapManifestBumpError("watchmaster.propose_spawn", err)
		}
		return manifestBumpResult{ManifestVersionID: result.ManifestVersionID, VersionNo: result.VersionNo}, nil
	}
}

// ── adjust_personality ───────────────────────────────────────────────────────

// adjustPersonalityParams is the wire shape decoded from the
// `watchmaster.adjust_personality` params field.
type adjustPersonalityParams struct {
	AgentID        string `json:"agent_id"`
	NewPersonality string `json:"new_personality"`
	ApprovalToken  string `json:"approval_token"`
}

// NewAdjustPersonalityHandler returns a [MethodHandler] that implements
// `watchmaster.adjust_personality`. Closes over `deps`.
func NewAdjustPersonalityHandler(deps WatchmasterWriteDeps) MethodHandler {
	checkWriteDeps("NewAdjustPersonalityHandler", deps)
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		if len(params) == 0 {
			return nil, NewRPCError(ErrCodeInvalidParams,
				"watchmaster.adjust_personality: params must not be null")
		}
		var p adjustPersonalityParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(ErrCodeInvalidParams,
				fmt.Sprintf("watchmaster.adjust_personality: decode params: %v", err))
		}
		claim := deps.ResolveClaim(ctx)
		result, err := spawn.AdjustPersonality(ctx, deps.Client, deps.Logger, spawn.AdjustPersonalityRequest{
			AgentID:        p.AgentID,
			NewPersonality: p.NewPersonality,
			ApprovalToken:  p.ApprovalToken,
		}, claim)
		if err != nil {
			return nil, mapManifestBumpError("watchmaster.adjust_personality", err)
		}
		return manifestBumpResult{ManifestVersionID: result.ManifestVersionID, VersionNo: result.VersionNo}, nil
	}
}

// ── adjust_language ──────────────────────────────────────────────────────────

// adjustLanguageParams is the wire shape decoded from the
// `watchmaster.adjust_language` params field.
type adjustLanguageParams struct {
	AgentID       string `json:"agent_id"`
	NewLanguage   string `json:"new_language"`
	ApprovalToken string `json:"approval_token"`
}

// NewAdjustLanguageHandler returns a [MethodHandler] that implements
// `watchmaster.adjust_language`. Closes over `deps`.
func NewAdjustLanguageHandler(deps WatchmasterWriteDeps) MethodHandler {
	checkWriteDeps("NewAdjustLanguageHandler", deps)
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		if len(params) == 0 {
			return nil, NewRPCError(ErrCodeInvalidParams,
				"watchmaster.adjust_language: params must not be null")
		}
		var p adjustLanguageParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, NewRPCError(ErrCodeInvalidParams,
				fmt.Sprintf("watchmaster.adjust_language: decode params: %v", err))
		}
		claim := deps.ResolveClaim(ctx)
		result, err := spawn.AdjustLanguage(ctx, deps.Client, deps.Logger, spawn.AdjustLanguageRequest{
			AgentID:       p.AgentID,
			NewLanguage:   p.NewLanguage,
			ApprovalToken: p.ApprovalToken,
		}, claim)
		if err != nil {
			return nil, mapManifestBumpError("watchmaster.adjust_language", err)
		}
		return manifestBumpResult{ManifestVersionID: result.ManifestVersionID, VersionNo: result.VersionNo}, nil
	}
}

// ── shared ───────────────────────────────────────────────────────────────────

// checkWriteDeps centralises the panic-on-nil-dependency guards the
// three manifest-bump constructors share. The constructor name is
// embedded in the panic message so a boot-time mis-wiring surfaces a
// scannable stack trace.
func checkWriteDeps(ctorName string, deps WatchmasterWriteDeps) {
	if deps.Client == nil {
		panic(fmt.Sprintf("harnessrpc: %s: client must not be nil", ctorName))
	}
	if deps.Logger == nil {
		panic(fmt.Sprintf("harnessrpc: %s: logger must not be nil", ctorName))
	}
	if deps.ResolveClaim == nil {
		panic(fmt.Sprintf("harnessrpc: %s: claim resolver must not be nil", ctorName))
	}
}

// mapManifestBumpError translates a [spawn] sentinel into the matching
// JSON-RPC error code. Mirrors M6.1.b mapSpawnError verbatim — the
// only delta is the `methodName` parameter so the wire message echoes
// the right method for debuggability across all three handlers.
func mapManifestBumpError(methodName string, err error) error {
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
