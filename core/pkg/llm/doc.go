// Package llm defines the portable [Provider] interface and the value
// types its methods exchange. ROADMAP §M5 → M5.2.
//
// An LLM provider is the thing that turns a sequence of [Message]
// values into a model response: completing a turn, streaming a turn as
// it generates, counting tokens for budgeting, and reporting usage
// after a turn for cost accounting. M5.2 covers ONLY the interface and
// its value types. The Claude-Code-backed default implementation (via
// the Claude Agent SDK if embedding, or as a subprocess if shelling
// out) lands in a follow-up sub-package — proposed split M5.2.a
// (interface, this PR) / M5.2.b (Claude Code default impl).
//
// # Why a portable interface
//
// Phase 1 ships with Claude Code as the first provider (M5.2.b) but
// the design must accommodate alternative providers without
// refactoring callers. The split is therefore:
//
//   - This package owns the interface, the value types, and the
//     sentinel-error vocabulary every provider must speak.
//   - Each concrete provider sub-package implements [Provider] and
//     translates its native completion / streaming / usage surface
//     into the value types defined here.
//   - Higher-level orchestration (the [runtime.AgentRuntime]
//     implementations from M5.3+, the cost tracker from M6.3) depends
//     on [Provider] and never imports a concrete provider package
//     directly. M5.10's provider-swap conformance test exercises this
//     contract.
//
// The interface is intentionally small (four methods) and avoids
// provider-specific concepts (no Anthropic content blocks, no OpenAI
// function-calling envelope, no Google safety attributes). Where a
// concept does not portably translate the type uses a `map[string]string`
// or `map[string]any` metadata bag the provider populates and consumes
// opaquely. The metadata-maps discipline mirrors the M4.1 [messenger]
// adapter pattern and the M5.1 [runtime] interface documented in
// `docs/LESSONS.md`.
//
// # Method surface
//
// The four methods reflect the lifecycle of a single LLM turn — count
// tokens before sending (budgeting), complete the turn synchronously or
// stream it as it generates, and report usage after — plus the cost
// hook that downstream cost-tracking (M6.3) consumes:
//
//   - [Provider.Complete]     — synchronous completion; one request,
//     one response with usage attached.
//   - [Provider.Stream]       — streaming completion; events delivered
//     to a callback as the model generates.
//   - [Provider.CountTokens]  — deterministic token count for a
//     prospective request, no model invocation.
//   - [Provider.ReportCost]   — record post-turn usage against a
//     [runtime.AgentRuntime] session id; the cost accumulator (M6.3)
//     subscribes via the provider's bookkeeping.
//
// Synchronous validation runs first on every method: empty
// [CompleteRequest.Model] / [StreamRequest.Model] /
// [CountTokensRequest.Model] surface [ErrModelNotSupported] when the
// caller supplies a model the provider does not handle; an empty
// messages list surfaces [ErrInvalidPrompt]; a nil handler on
// [Provider.Stream] surfaces [ErrInvalidHandler]. The underlying
// provider / model is NEVER contacted on these paths, so the sentinels
// are safe to log and act on.
//
// # Lifecycle of a turn
//
// A canonical Complete turn looks like:
//
//  1. Caller assembles a [CompleteRequest] (model id, system prompt,
//     ordered [Message]s, optional tool definitions).
//  2. Caller optionally calls [Provider.CountTokens] to budget-check.
//  3. Caller calls [Provider.Complete]; provider drives the model and
//     returns a [CompleteResponse] with usage populated.
//  4. Caller calls [Provider.ReportCost] with the runtime session id
//     and the [Usage] from the response.
//
// A canonical Stream turn looks like:
//
//  1. Caller assembles a [StreamRequest] and a [StreamHandler].
//  2. Caller calls [Provider.Stream]; provider opens the upstream
//     connection and dispatches [StreamEvent]s to the handler.
//  3. Caller observes [StreamEventKindMessageStop] (final usage rides
//     on the stop event), then calls [StreamSubscription.Stop].
//  4. Caller calls [Provider.ReportCost] with the runtime session id
//     and the [Usage] observed on the stop event.
//
// # Provider-specific concepts via metadata
//
// Real LLM APIs expose features that don't portably map: Anthropic's
// `system` as a separate top-level field vs in-messages, OpenAI's
// `logprobs`, Google's `safetySettings`, etc. The interface uses
// [Message] for the conversational sequence and a separate
// [CompleteRequest.System] / [StreamRequest.System] string for the
// system prompt — Anthropic's shape, since that's the Phase-1 provider.
// Future providers that want messages-only systems compose the
// [Message] sequence themselves with a synthetic system role. Knobs
// outside the portable shape ride on [CompleteRequest.Metadata] /
// [StreamRequest.Metadata]; providers consume only keys they recognise.
//
// # Subscription lifecycle (Stream)
//
// [Provider.Stream] returns a [StreamSubscription] handle. The handler
// runs in a goroutine the provider owns; concurrency limits and
// ordering guarantees are provider-specific. Callers stop receiving
// events by calling [StreamSubscription.Stop]; Stop is idempotent and
// blocks until the in-flight handler returns. Mirrors the
// [runtime.Subscription] / [messenger.Subscription] shape so callers
// wiring all three surfaces share a mental model.
//
// Phase 1 is at-most-once at this layer: a non-nil error returned from
// the handler is logged by the provider but does NOT redeliver the
// event. Durable redelivery lives in the M3.7 outbox upstream. A
// handler error surfaces from [Provider.Stream] only as the cause of
// stream termination, after which subsequent [StreamSubscription.Stop]
// calls return [ErrStreamClosed] wrapping the original cause.
//
// # Type opacity
//
// [Model] is a string alias so callers can pass model identifiers
// across boundaries without import cycles, but the bytes themselves
// are provider-defined (Anthropic uses `claude-sonnet-4`, OpenAI uses
// `gpt-4o`, etc.). Code that needs to inspect or reconstruct model
// ids belongs in the provider sub-package, not here.
//
// Metadata maps on [CompleteRequest], [CompleteResponse],
// [StreamRequest], [StreamEvent], [Message], and [Usage] carry
// provider-specific extensions. The interface package never inspects
// them.
//
// The M2 cross-cutting constraint applies here too: payloads MUST NOT
// carry infrastructure metadata (`deployment_id`, `environment`,
// `host`, `pod`, …). Provider metadata is for provider-internal
// context, not for shipping operational telemetry.
//
// # Out of scope (deferred)
//
//   - Concrete provider implementations — the Claude-Code-backed
//     default lives in the M5.2.b follow-up. This package is types +
//     interface only.
//   - Tool execution — the [runtime.AgentRuntime] surface owns tool
//     invocation (M5.1); a [Provider] only surfaces tool-call requests
//     emitted by the model via [StreamEventKindToolCallStart] /
//     [StreamEventKindToolCallDelta] events and the
//     [CompleteResponse.ToolCalls] slice. Execution is the runtime's
//     job.
//   - Provider credentials — M5.9 routes Claude Code credentials via
//     the secrets interface; the provider consumes whatever credentials
//     the M5.2.b implementation negotiates. This package never names a
//     credential.
//   - Provider-swap conformance test — M5.10 exercises the contract a
//     [FakeProvider] (test-only, in `fake_provider_test.go`) and the
//     Claude Code provider both must pass.
//   - Cost catalogue — concrete cost-per-token rates and aggregation
//     (M6.3 cost tracker) live downstream; this package's
//     [Provider.ReportCost] is the ingest point, not the accumulator.
package llm
