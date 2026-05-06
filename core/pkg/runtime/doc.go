// Package runtime defines the portable [AgentRuntime] interface and the
// value types its methods exchange. ROADMAP §M5 → M5.1.
//
// An agent runtime is the thing that drives a Watchkeeper's LLM
// session: it boots a session from a [Manifest], feeds it messages,
// executes tool invocations on the agent's behalf, streams events back
// to the orchestrator, and tears it down. M5.1 covers ONLY the
// interface and its value types. The Claude-Code-via-TS-harness runtime
// (M5.3) lives in `core/pkg/runtime/...`-style sibling sub-packages or
// a dedicated `harness/` package, layered on top of this surface;
// future runtimes (in-process Go, embedded SDK, fakes) implement the
// same interface.
//
// # Why a portable interface
//
// Phase 1 ships with the Claude Code TS harness as the first runtime
// (M5.3) but the design must accommodate alternative runtimes without
// refactoring callers. The split is therefore:
//
//   - This package owns the interface, the value types, and the
//     sentinel-error vocabulary every runtime must speak.
//   - Each concrete runtime sub-package implements [AgentRuntime] and
//     translates its native session / tool / event surface into the
//     value types defined here.
//   - Higher-level orchestration (lifecycle, watchkeeper handlers, the
//     M6 Watchmaster) depends on [AgentRuntime] and never imports a
//     concrete runtime package directly.
//
// The interface is intentionally small (five methods) and avoids
// runtime-specific concepts (no Claude Code session id, no isolate-vm
// handle, no JSON-RPC envelope). Where a concept does not portably
// translate the type uses a `map[string]string` or `map[string]any`
// metadata bag the runtime populates and consumes opaquely. The
// metadata-maps discipline mirrors the M4.1 [messenger] adapter
// pattern documented in `docs/LESSONS.md`.
//
// # Method surface
//
// The five methods reflect the lifecycle of an agent session — provision
// the session, feed it human input, execute tools on its behalf,
// observe streaming events, tear it down:
//
//   - [AgentRuntime.Start]        — provision a session from a
//     [Manifest]; returns a [Runtime] handle whose [Runtime.ID] the
//     caller passes to subsequent methods.
//   - [AgentRuntime.SendMessage]  — feed `msg` as a user-turn input.
//   - [AgentRuntime.InvokeTool]   — run a tool the agent requested
//     (or a tool the orchestrator pre-empted).
//   - [AgentRuntime.Subscribe]    — receive [Event] notifications
//     (agent message, tool call, tool result, runtime error).
//   - [AgentRuntime.Terminate]    — end the session and release
//     resources.
//
// Synchronous validation runs first on every method: empty
// [Manifest.AgentID] / [Manifest.SystemPrompt] / [Manifest.Model]
// surface [ErrInvalidManifest]; empty [Message.Text] surfaces
// [ErrInvalidMessage]; empty [ToolCall.Name] surfaces
// [ErrInvalidToolCall]; nil handler surfaces [ErrInvalidHandler]. The
// underlying runtime / LLM / tool is NEVER contacted on these paths,
// so the sentinels are safe to log and act on.
//
// # Lifecycle of a session
//
// A canonical session looks like:
//
//  1. Caller resolves a [Manifest] (M5.5 will project a
//     `keepclient.ManifestVersion` into a runtime [Manifest]).
//  2. Caller calls [AgentRuntime.Start], receives a [Runtime] handle.
//  3. Caller calls [AgentRuntime.Subscribe] with an [EventHandler]
//     that observes [EventKindMessage] / [EventKindToolCall] /
//     [EventKindToolResult] / [EventKindError] events.
//  4. Caller calls [AgentRuntime.SendMessage] with a user prompt; the
//     runtime drives the LLM, emits events, possibly invokes tools.
//  5. The orchestrator calls [AgentRuntime.InvokeTool] when a tool
//     request requires policy mediation (capability check, lead-
//     approval gate, etc.) before execution. A runtime that drives
//     tools end-to-end internally still surfaces the call/result via
//     Subscribe.
//  6. Caller calls [AgentRuntime.Terminate] when the session is done.
//
// # Manifest source
//
// The [Manifest] type here is the runtime-facing shape, not the
// wire-format the Keep server stores. M5.5 owns the loader that maps
// `keepclient.ManifestVersion` (the wire shape with raw-JSON tools and
// authority_matrix) into this typed [Manifest] (with
// [Manifest.SystemPrompt] already templated from Personality and
// Language, [Manifest.Toolset] decoded from the tools jsonb,
// [Manifest.AuthorityMatrix] projected from the authority_matrix jsonb).
// The runtime trusts the loader's output and does not re-template.
//
// Defining the runtime-facing [Manifest] locally (rather than aliasing
// `keepclient.ManifestVersion`) keeps the runtime decoupled from the
// wire schema's evolution: when the server adds a column, the
// keepclient surface grows; this package's [Manifest] only changes
// when the runtime needs the new field. The mapping is one-way:
// loader → runtime [Manifest]; the runtime never round-trips a
// [Manifest] back to the wire shape.
//
// # Subscribe lifecycle
//
// [AgentRuntime.Subscribe] returns a [Subscription] handle. The
// handler runs in a goroutine the runtime owns; concurrency limits and
// ordering guarantees are runtime-specific (the M5.3 TS-harness
// runtime serializes events per-session). Callers stop receiving
// events by calling [Subscription.Stop]; Stop is idempotent and blocks
// until the in-flight handler returns. The handler MUST be
// non-blocking on the runtime's terms; runtimes MAY surface their
// per-runtime timing as a documentation contract.
//
// Phase 1 is at-most-once at this layer: a non-nil error returned from
// the handler is logged by the runtime but does NOT redeliver the
// event. Durable redelivery lives in the M3.7 outbox upstream.
//
// # Type opacity
//
// [ID] is a string alias so callers can pass it across
// boundaries without import cycles, but the bytes themselves are
// runtime-defined. Code that needs to inspect or reconstruct ids
// belongs in the runtime sub-package, not here.
//
// Metadata maps on [Manifest], [Message], [ToolCall], [ToolResult],
// [Event], and [StartOptions] carry runtime-specific extensions
// (TS-harness module path, isolate options, channel id, sender
// platform id, …). The interface package never inspects them.
//
// The M2 cross-cutting constraint applies here too: payloads MUST NOT
// carry infrastructure metadata (`deployment_id`, `environment`,
// `host`, `pod`, …). Runtime metadata is for runtime-internal context,
// not for shipping operational telemetry.
//
// # Sandbox guardrail surface
//
// [SandboxRunner] (M5.4.a) wraps an `os/exec` subprocess with two
// guardrails: a wall-clock timeout (via [time.AfterFunc] +
// [exec.Cmd.Process.Kill]) and an output-byte cap (via wrapped
// [io.Writer] adapters that count cumulative stdout+stderr bytes).
// Termination outcomes surface as [RunResult.TermReason] using the
// exported `TermReason*` constants — `natural`, `wall_clock`,
// `output_cap`, `context_canceled`. Any sandbox-driven kill returns
// an error wrapping [ErrSandboxKilled]; natural exits (including
// non-zero exit codes) return a nil error so the caller can treat
// "process ran" and "process was terminated by us" as distinct
// signals. The leaf is syscall-free and cross-platform across Linux
// and Darwin.
//
// CPU-time and memory-ceiling rlimits are deferred to M5.4.b because
// they require platform-specific `setrlimit` plumbing
// (`syscall.SysProcAttr.Rlimit`) and carry CI-flake risk that
// warrants a dedicated review. M5.4.a is the syscall-free
// foundation; M5.4.b layers the rlimit-driven guardrails on top
// without changing the [SandboxRunner.Run] return contract.
//
// # Out of scope (deferred)
//
//   - Concrete runtime implementations — the Claude-Code-via-TS-harness
//     runtime is M5.3, the per-tool resource-limit enforcer is M5.4,
//     the manifest loader is M5.5. This package is types + interface
//     only.
//   - LLM provider abstraction — see M5.2 [LLMProvider]; a runtime
//     wraps a provider, but the two surfaces are distinct.
//   - Tool execution sandbox — `isolated-vm` (pure-JS tools) and
//     worker-process (I/O-capable tools) are M5.3 / M5.4 concerns;
//     this package only defines the [ToolCall] / [ToolResult] shapes
//     the runtime exchanges.
//   - Notebook auto-recall — M5.6 wires per-agent SQLite into the
//     harness; the runtime's job is to expose `Remember` as a tool,
//     not to manage the store.
//   - Auto-reflection on tool error — M5.7 layers reflection on top
//     of the runtime's tool error surface ([ToolResult.Error]).
//   - Provider credentials — M5.9 routes Claude Code credentials via
//     the secrets interface; the runtime consumes whatever provider
//     the caller hands it (M5.10 conformance test).
//   - Capability-token enforcement on [AgentRuntime.InvokeTool] — token
//     issuance is the [capability] package's job (M3.5); runtime-side
//     wiring is deferred to M5.3 where call sites are concrete.
package runtime
