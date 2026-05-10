package runtime

import "errors"

// ErrInvalidManifest is returned synchronously by [AgentRuntime.Start]
// when the supplied [Manifest] fails validation (empty AgentID, empty
// SystemPrompt, empty Model). Distinguishes "the manifest is malformed
// and the runtime never spawned a session" from "the runtime tried but
// failed". Matchable via [errors.Is].
var ErrInvalidManifest = errors.New("runtime: invalid manifest")

// ErrInvalidMessage is returned synchronously by
// [AgentRuntime.SendMessage] when [Message.Text] is empty. The
// underlying runtime is NOT touched on this path. Matchable via
// [errors.Is].
var ErrInvalidMessage = errors.New("runtime: invalid message")

// ErrInvalidToolCall is returned synchronously by
// [AgentRuntime.InvokeTool] when [ToolCall.Name] is empty. The
// underlying runtime / tool is NOT touched on this path. Matchable
// via [errors.Is].
var ErrInvalidToolCall = errors.New("runtime: invalid tool call")

// ErrInvalidHandler is returned synchronously by
// [AgentRuntime.Subscribe] when the supplied [EventHandler] is nil. A
// nil handler is a programmer error at the call site; surfacing the
// sentinel rather than panicking lets the caller recover and report.
// Matchable via [errors.Is].
var ErrInvalidHandler = errors.New("runtime: invalid handler")

// ErrRuntimeNotFound is returned by [AgentRuntime.SendMessage] /
// [AgentRuntime.InvokeTool] / [AgentRuntime.Subscribe] when the
// supplied [ID] does not match a live session. Distinct from
// [ErrTerminated] — the former means the id was never minted, the
// latter means the id was minted but the session has since been
// terminated. Matchable via [errors.Is].
var ErrRuntimeNotFound = errors.New("runtime: runtime not found")

// ErrTerminated is returned by [AgentRuntime.SendMessage] /
// [AgentRuntime.InvokeTool] / [AgentRuntime.Subscribe] after the
// session was terminated via [AgentRuntime.Terminate]. The handle id
// remains a valid lookup key for the lifetime of the runtime
// implementation's bookkeeping; reusing it surfaces this sentinel
// rather than [ErrRuntimeNotFound] so callers can distinguish "stale
// pointer" from "wrong pointer". Matchable via [errors.Is].
var ErrTerminated = errors.New("runtime: terminated")

// ErrToolUnauthorized is returned by [AgentRuntime.InvokeTool] when the
// requested tool name is absent from the session manifest's
// [Manifest.Toolset]. This is the runtime-side enforcement of the
// capability ACL the manifest declares; it is NOT a substitute for the
// capability-token validation that happens upstream in
// [capability.Broker.Validate]. Matchable via [errors.Is].
var ErrToolUnauthorized = errors.New("runtime: tool unauthorized")

// ErrSandboxKilled is returned by [SandboxRunner.Run] when the
// sandboxed process was terminated by one of the guardrail paths
// (wall-clock timeout, output-byte cap, or context cancellation).
// Natural exits — including non-zero exit codes — return a nil error;
// callers inspect [RunResult.ExitCode] to react to those. The wrapping
// chain preserves the underlying termination cause: context-cancel
// kills wrap [context.Canceled] / [context.DeadlineExceeded] alongside
// this sentinel so the call site can `errors.Is` either signal.
// Matchable via [errors.Is].
var ErrSandboxKilled = errors.New("runtime: sandbox killed")

// ErrSubscriptionClosed is returned by [Subscription.Stop] when the
// dispatch loop exited with a transport error before Stop was called
// (the wrapped error rides via the [errors.Is] chain). A clean
// shutdown returns nil. Mirrors [messenger.ErrSubscriptionClosed].
// Matchable via [errors.Is].
var ErrSubscriptionClosed = errors.New("runtime: subscription closed")

// ErrEmbedderRequired is returned by [NewToolErrorReflector] when no
// [llm.EmbeddingProvider] was supplied via [WithEmbedder]. The
// reflector composes a [notebook.Entry] of category `lesson` and
// MUST embed it before calling Remember; there is no sane default
// embedder to fall back to (silently no-op'ing every call would mask
// the bug). Matches the existing runtime sentinel idioms; matchable
// via [errors.Is].
var ErrEmbedderRequired = errors.New("runtime: embedder required")

// ErrAgentNotOpened is returned by [NotebookSupervisor.BootCheck] when
// the supplied `agentID` does not have a live [notebook.DB] handle in
// the supervisor's registry. Callers reach BootCheck during a runtime
// boot path that expects the per-agent notebook to have been Opened
// already; a missing entry is a wiring bug, not a transient error.
// Distinct from [ErrRuntimeNotFound] / [ErrTerminated] which describe
// the agent runtime session lifecycle — BootCheck operates one layer
// below, on the per-agent SQLite handle. Matchable via [errors.Is].
var ErrAgentNotOpened = errors.New("runtime: agent notebook not opened")

// ErrApprovalRequired is returned by [ToolDispatcher.Dispatch] (M8.2.a)
// when the manifest's [RequiresApproval] gate denies the call. The
// concrete error carries the action name and the reason string the
// gate produced; callers that need the reason text use
// [errors.As] to unwrap an [*ApprovalRequiredError]; callers that only
// need to short-circuit on "approval required" use
// [errors.Is](err, [ErrApprovalRequired]). Matchable via [errors.Is].
var ErrApprovalRequired = errors.New("runtime: approval required")

// ErrToolHandlerMissing is returned by [ToolDispatcher.Dispatch] (M8.2.a)
// when the call's tool name passes the [Manifest.Toolset] gate and the
// [RequiresApproval] gate but no handler has been registered for it
// via [ToolDispatcher.Register]. Distinguishes "the manifest declares
// a tool but the runtime hasn't wired its handler" from
// [ErrToolUnauthorized] ("the manifest doesn't declare the tool at
// all"). Surfaces as a wiring bug — production should never hit it
// once the M8.2.a..d sub-items have all landed. Also returned by
// [ToolDispatcher.Register] when the supplied handler is nil.
// Matchable via [errors.Is].
var ErrToolHandlerMissing = errors.New("runtime: tool handler missing")

// ApprovalRequiredError is the concrete error type returned by
// [ToolDispatcher.Dispatch] when the [RequiresApproval] gate denies
// the call. Carries the tool action name and the human-readable
// reason the gate produced. Satisfies [errors.Is]([ErrApprovalRequired])
// so callers that only need to short-circuit can use the sentinel
// idiom; callers that need to render the reason use [errors.As] to
// unwrap.
type ApprovalRequiredError struct {
	// Action is the [ToolCall.Name] the dispatcher refused to run.
	Action string

	// Reason is the human-readable string [RequiresApproval] returned
	// alongside the deny decision (e.g.
	// `"authority matrix requires lead approval for update_ticket_field"`).
	Reason string
}

// Error returns a stable rendering of the approval-required denial
// suitable for logging and surfacing to the agent. Format:
// `"runtime: approval required for <action>: <reason>"`.
func (e *ApprovalRequiredError) Error() string {
	return "runtime: approval required for " + e.Action + ": " + e.Reason
}

// Is reports whether `target` is the [ErrApprovalRequired] sentinel.
// Lets callers do `errors.Is(err, ErrApprovalRequired)` without
// caring about the concrete type. Other targets fall through to the
// default chain comparison.
func (e *ApprovalRequiredError) Is(target error) bool {
	return target == ErrApprovalRequired
}

// ErrUnsupportedPlatform is returned by [SandboxRunner.Run] (M5.4.b)
// when the caller asked for a non-zero CPU-time or memory-ceiling
// rlimit on a platform whose `applyRlimits` shim does not implement
// kernel enforcement. Linux returns nil from the shim; Darwin returns
// nil and silently does not enforce; every other platform surfaces
// this sentinel only when at least one rlimit field is non-zero. A
// fully zeroed [SandboxConfig] never trips this error. Matchable via
// [errors.Is].
var ErrUnsupportedPlatform = errors.New("runtime: rlimits unsupported on this platform")
