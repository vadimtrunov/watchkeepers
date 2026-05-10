package runtime

import (
	"context"
	"sync"
)

// ToolHandler is the per-tool function the [ToolDispatcher] dispatches
// to once a [ToolCall] passes the manifest gates. The signature mirrors
// [AgentRuntime.InvokeTool]'s return contract: tool-side failures that
// did not abort the runtime ride on [ToolResult.Error]; transport /
// authorisation / programmer errors surface as the returned `error`.
//
// Handlers MUST honour `ctx` (return `ctx.Err()` when cancelled).
// Implementations are typically constructed from per-tool factory
// helpers (e.g. `coordinator.NewUpdateTicketFieldHandler(jiraClient)`
// from M8.2.a) so the handler closes over the dependencies it needs
// without polluting the dispatcher API.
type ToolHandler func(ctx context.Context, call ToolCall) (ToolResult, error)

// ToolDispatcher gates a [ToolCall] against a [Manifest]'s
// [Manifest.Toolset] and authority matrix and dispatches to a
// registered [ToolHandler] once the gates pass. Construct via
// [NewToolDispatcher]; the zero value is not usable. Methods are safe
// for concurrent use after construction.
//
// The dispatcher is intentionally NOT an [AgentRuntime] — it does not
// own a session lifecycle, cannot Start / SendMessage / Subscribe /
// Terminate. M8.2.a wires the dispatcher into the M8.2.b+ Coordinator
// boot path; future runtime integrations (M6.2 Watchmaster toolset,
// M9 multi-source tool registry) consume the same primitive without
// re-implementing the gate sequence.
//
// Known limitation (deferred to M9): routing is strictly NAME-based.
// [ToolCall.ToolVersion] (carried for the M5.6.b reflector lesson
// scoping) is NOT consulted by the dispatcher — a single name maps
// to a single handler regardless of version. The M9 multi-source
// tool registry will need a versioned variant (e.g. a
// `RegisterVersioned(name, version, handler)` surface plus a
// per-(name,version) lookup) when same-name tools across sources
// require incompatible handler behaviour. The M8.2.a..d toolset has
// one handler per name and so does not exercise this limitation.
//
// Gate sequence (in order; each step short-circuits):
//
//  1. `ctx.Err() != nil` → return `ctx.Err()` (no map work).
//  2. `call.Name == ""` → [ErrInvalidToolCall].
//  3. `call.Name` ∉ `manifest.Toolset.Names()` → [ErrToolUnauthorized].
//  4. [RequiresApproval] returns `(true, reason)` →
//     [*ApprovalRequiredError] (matchable via
//     [errors.Is]([ErrApprovalRequired])).
//  5. No handler registered for `call.Name` → [ErrToolHandlerMissing].
//  6. Dispatch to the registered handler; return its
//     `(ToolResult, error)` verbatim.
//
// The gate ordering is deliberate: the membership check (step 3) runs
// BEFORE the approval gate (step 4) so a manifest typo surfaces as
// "unauthorised" rather than "needs approval". The handler-presence
// check (step 5) runs AFTER the approval gate so a missing handler in
// production cannot leak through an approval bypass.
type ToolDispatcher struct {
	mu       sync.RWMutex
	handlers map[string]ToolHandler
}

// NewToolDispatcher constructs a [ToolDispatcher] with an empty
// handler registry. The dispatcher has no required dependency — pass
// no options. Register handlers via [ToolDispatcher.Register] before
// calling [ToolDispatcher.Dispatch]. A dispatcher with zero handlers
// is valid; every [ToolDispatcher.Dispatch] call surfaces
// [ErrToolHandlerMissing] (step 5 of the gate sequence).
func NewToolDispatcher() *ToolDispatcher {
	return &ToolDispatcher{
		handlers: make(map[string]ToolHandler),
	}
}

// Register associates `handler` with the manifest tool name `name`.
// A second call for the same name overrides the previous handler.
//
// Validation:
//
//   - Empty `name` → [ErrInvalidToolCall].
//   - Nil `handler` → [ErrToolHandlerMissing].
//
// Safe for concurrent use; the registry is guarded by a sync.RWMutex.
// Callers that wire their handlers at boot before any dispatch will
// not contend on the lock.
func (d *ToolDispatcher) Register(name string, handler ToolHandler) error {
	if name == "" {
		return ErrInvalidToolCall
	}
	if handler == nil {
		return ErrToolHandlerMissing
	}
	d.mu.Lock()
	d.handlers[name] = handler
	d.mu.Unlock()
	return nil
}

// Dispatch runs the gate sequence (documented on [ToolDispatcher])
// against `manifest` and `call`, then dispatches to the registered
// [ToolHandler] when all gates pass. Returns the handler's
// `(ToolResult, error)` verbatim on dispatch; otherwise returns the
// gate's sentinel.
//
// Concurrency: safe to call from multiple goroutines. Handler
// dispatch happens AFTER the read-lock is released so a handler that
// itself calls back into [ToolDispatcher.Register] / [ToolDispatcher.Dispatch]
// does not self-deadlock.
func (d *ToolDispatcher) Dispatch(
	ctx context.Context, manifest Manifest, call ToolCall,
) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if call.Name == "" {
		return ToolResult{}, ErrInvalidToolCall
	}

	if !toolsetContains(manifest.Toolset, call.Name) {
		return ToolResult{}, ErrToolUnauthorized
	}

	if required, reason := RequiresApproval(manifest, call.Name); required {
		return ToolResult{}, &ApprovalRequiredError{Action: call.Name, Reason: reason}
	}

	d.mu.RLock()
	handler, ok := d.handlers[call.Name]
	d.mu.RUnlock()
	if !ok {
		return ToolResult{}, ErrToolHandlerMissing
	}

	return handler(ctx, call)
}

// toolsetContains reports whether `name` is present in `toolset`. The
// manifest carries [Toolset] as an ordered slice (the loader does
// NOT sort — see `core/pkg/runtime/toolset.go`), so a linear scan is
// the correct shape; the typical Coordinator manifest carries fewer
// than ten tools so the cost is negligible relative to the handler
// dispatch that follows. Hoisted into a helper so the [Dispatch]
// method body reads top-to-bottom as a sequence of gates.
func toolsetContains(toolset Toolset, name string) bool {
	for _, entry := range toolset {
		if entry.Name == name {
			return true
		}
	}
	return false
}
