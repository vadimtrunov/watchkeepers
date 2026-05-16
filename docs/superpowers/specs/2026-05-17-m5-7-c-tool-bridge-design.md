# M5.7.c slice 4 — ClaudeAgentProvider tool bridge

**Status:** design, awaiting operator review.
**Branch:** `feat/m5.7.c-claude-agent-provider`.
**Predecessors on branch:** slices 1–3 (foundation + env-toggle + stream + cancellation).
**Successors on branch:** slice 5 (cost-tracking polish), slice 6 (parameterised
conformance), slice 7 (docs + roadmap close-out).
**Cross-cutting follow-up referenced (not in scope):** M5.3.c.c.c — inbound
`role=tool` translation for **both** providers.

## 1. Problem

`@anthropic-ai/claude-agent-sdk` owns tool execution by default: a registered
tool's handler runs inside the SDK's agentic loop and the host only observes
the result. The `LLMProvider` contract that `ClaudeCodeProvider` implements
takes the opposite stance — the provider returns `CompleteResponse.toolCalls`
unexecuted, and `runtime.AgentRuntime.InvokeTool` runs them outside the
provider (see `core/pkg/llm/provider.go:74` and
`harness/src/llm/provider.ts:53`).

For `ClaudeAgentProvider` to satisfy that contract — and stay a drop-in swap
for `ClaudeCodeProvider` per the M5.7.b conformance suite — the provider must
intercept tool-use requests **before** the SDK dispatches them, return them
to the runtime, and never let the SDK execute Watchkeeper tools itself.

## 2. Goals / non-goals

### In scope (slice 4)

- Outbound interception of `tool_use` content blocks inside SDK-emitted
  `SDKAssistantMessage` frames, returned as `CompleteResponse.toolCalls`.
- The same interception in `stream()`, surfaced through `StreamEvent`s
  (`tool_call_start`, `tool_call_delta`, `message_stop` with
  `finishReason: "tool_use"`).
- Parallel tool-use support: multiple `tool_use` blocks emitted in a single
  assistant message all land in `toolCalls[]` in their original order.
- Tool-name translation: Watchkeeper's `notebook.remember` style runtime
  names become MCP-legal `notebook_remember`, are prefixed by the SDK as
  `mcp__watchkeeper__notebook_remember`, and round-trip back to
  `notebook.remember` when the model emits a `tool_use`.
- Safety net: if the SDK ever calls a stub MCP handler (race window), the
  provider raises `LLMError.providerUnavailable` with a diagnostic message
  rather than silently swallowing the escape.
- Unit-level test coverage for the three new components and a small set of
  integration cases on the existing `queryImpl` DI seam.

### Out of scope (deferred)

- Inbound `role=tool` translation. `ClaudeCodeProvider` skips `role=tool`
  today (`harness/src/llm/claude-code-provider.ts:337`, comment refers to
  M5.3.c.c.c). `ClaudeAgentProvider` mirrors that skip with the same
  cross-reference. The cross-cutting fix lands once for both providers in
  M5.3.c.c.c.
- Cost-tracking polish — slice 5.
- Cross-provider parameterised conformance — slice 6.
- Documentation / roadmap close-out — slice 7.
- Multi-turn agentic loops driven from a single `complete()` call. The
  provider preserves the one-turn contract; multi-turn orchestration stays
  with the runtime.

## 3. Architecture

Slice 4 grows `harness/src/llm/` by three small modules that the existing
`claude-agent-provider.ts` composes. No public `LLMProvider` surface changes.

```
harness/src/llm/
  claude-agent-provider.ts          # existing, extended
  tool-bridge/
    name-codec.ts                   # runtime-name <-> mcp-name bijection
    mcp-stub-server.ts              # createSdkMcpServer with throw-stubs
    interceptor.ts                  # iterator-watching + interrupt
```

The bridge is **outbound-only** in this slice; inbound handling stays at
the existing `buildPromptFromMessages`, which still skips `role=tool` with
a `// M5.3.c.c.c` cross-reference.

### 3.1 Component: `name-codec.ts`

Builds a bijective map between runtime tool names and MCP-legal names once
per `complete()` / `stream()` call from `req.tools`.

```ts
export interface ToolNameCodec {
  encode(runtimeName: string): string;
  decode(mcpName: string): string;
}

export function buildCodec(tools: readonly ToolDefinition[]): ToolNameCodec;
```

- **Encoding rule:** `s.replaceAll(".", "_")`.
- **Collision detection:** if two distinct runtime names sanitise to the
  same MCP name (e.g. `a.b` and `a_b`), `buildCodec` throws
  `LLMError.invalidPrompt("watchkeeper MCP name collision: <orig1>, <orig2> → <encoded>")`.
- **Decoding:** accepts both the bare encoded name and the SDK-prefixed
  form `mcp__watchkeeper__<encoded>`. Unknown names throw
  `LLMError.providerUnavailable("agent SDK emitted unknown tool name: <name>")`
  — an invariant violation, not an expected user-visible error.

The MCP server name (`watchkeeper`) is a string constant exported from this
module so the interceptor and the stub-server share a single source of truth.

### 3.2 Component: `mcp-stub-server.ts`

Constructs an in-process MCP server via the SDK's `createSdkMcpServer` API:

```ts
export function buildStubMcpServer(
  tools: readonly ToolDefinition[],
  codec: ToolNameCodec,
): McpSdkServerConfigWithInstance;
```

- Each tool is registered with its **encoded** name, the runtime
  `description`, and a Zod schema derived from `inputSchema`.
- Every handler is a stub that throws an `Error` whose message begins with
  the sentinel `"watchkeeper MCP stub invoked"`. The interceptor detects
  that sentinel and converts it to `LLMError.providerUnavailable`.
- **Schema conversion:** a minimal JSON-Schema → Zod converter. Supported
  types: `object` (with `properties` / `required`), `string`, `number`,
  `boolean`, `array`. Unknown shapes fall back to `z.any()` with a single
  warning per build. This is sufficient for Phase 1 manifests; a richer
  converter is future work outside this slice.

### 3.3 Component: `interceptor.ts`

Owns the consumption loop of the SDK iterator:

```ts
export interface InterceptedTurn {
  readonly toolCalls: readonly ToolCall[];
  readonly text: string;
  readonly finishReason: FinishReason;
  readonly usage: Usage | undefined;
  readonly errorMessage: string | undefined;
}

export async function interceptComplete(
  iter: ReturnType<typeof query>,
  codec: ToolNameCodec,
  requestedModel: Model,
): Promise<InterceptedTurn>;

export async function interceptStream(
  iter: ReturnType<typeof query>,
  handler: StreamHandler,
  codec: ToolNameCodec,
  requestedModel: Model,
  abortBag: { isStopped: boolean; markStopped(cause: unknown): void },
): Promise<void>;
```

- `interceptComplete` delegates per-message parsing to an extended
  `consumeAssistantMessage` that returns **all** `tool_use` blocks of one
  assistant message in array form (was a single `toolCall` field; becomes
  `toolCalls: ToolCall[]`).
- On the first assistant message with `toolCalls.length > 0`, the
  interceptor captures them, calls `iter.interrupt?.()`, and returns.
- If a `result` message arrives first (no tool use), the interceptor falls
  through the existing happy path (slice 1) and returns normally.
- `interceptStream` runs the same state machine but additionally dispatches
  `StreamEvent`s to the handler as `SDKPartialAssistantMessage` frames
  arrive (state already exists in slice 3). On the closing `message_stop`
  it emits `finishReason: "tool_use"` and calls `interrupt()`.

### 3.4 No `history-replay` module

The first revision of the design considered a dedicated `history-replay.ts`
that would serialise `Message[]` (including `role=tool`) into a structured
streaming-input prompt. We dropped it because `ClaudeCodeProvider` does not
fold `role=tool` either — that translation is M5.3.c.c.c's job. Slice 4
therefore keeps the existing flat `buildPromptFromMessages` (with a
`role === 'tool'` skip and `// M5.3.c.c.c` reference), preserving parity
with `ClaudeCodeProvider` and avoiding scope creep into a cross-cutting
slice.

## 4. Data flow

### 4.1 Outbound — `complete()` ending in tool_use

```
runtime → complete({ messages, tools })
  ↓
provider:
  codec  = buildCodec(tools)
  mcpSrv = buildStubMcpServer(tools, codec)
  prompt = buildPromptFromMessages(messages)        // unchanged
  opts   = { model, mcpServers: { watchkeeper: mcpSrv }, ... }
  iter   = query({ prompt, options: opts })
  turn   = await interceptComplete(iter, codec, model)
  ↓
SDK:
  assistant { content: [
    { type: "text",     text: "I'll check..." },
    { type: "tool_use", id: "tu_1",
      name: "mcp__watchkeeper__notebook_recall",  input: { q: "recent" } },
    { type: "tool_use", id: "tu_2",
      name: "mcp__watchkeeper__notebook_remember", input: { k: "summary" } }
  ] }
  ↓
interceptor:
  toolCalls = [
    { id: "tu_1", name: "notebook.recall",   arguments: { q: "recent" } },
    { id: "tu_2", name: "notebook.remember", arguments: { k: "summary" } }
  ]
  await iter.interrupt()                            // SDK loop unwinds; stubs untouched
  ↓
provider returns:
  { content: "I'll check...", toolCalls,
    finishReason: "tool_use", usage }
```

### 4.2 Outbound — `complete()` ending in plain `end_turn`

No `tool_use` block ever arrives; the interceptor falls through to the
slice-1 happy path: accumulate text, capture `result.usage`, return
`finishReason: "stop"`. Slice 4 does not regress slice 1.

### 4.3 Outbound — `stream()`

The state machine from slice 3 is preserved. Additions:

- `content_block_start` with `type: "tool_use"` emits `tool_call_start`
  and decodes the name from the MCP-prefixed form.
- `content_block_delta` with `type: "input_json_delta"` emits
  `tool_call_delta` correlated to the active tool id (already tracked).
- When the SDK emits a closing `message_stop` carrying
  `stop_reason: "tool_use"`, the interceptor emits a final
  `message_stop` event with `finishReason: "tool_use"`, then awaits
  `iter.interrupt()` and breaks the loop.
- All other `StreamEvent` flows from slice 3 (text deltas, error events,
  cancellation, handler-throw latching) are unchanged.

### 4.4 Race: stub handler invoked before interrupt

Between the iterator yielding an `SDKAssistantMessage` with `tool_use` and
the provider awaiting `iter.interrupt()`, the SDK may have already begun
its tool dispatch. The stub handler throws with a sentinel message.

If this happens, the SDK injects a `tool_result(is_error)` and proceeds to
the next assistant turn. The interceptor sees that next turn (or the
follow-up `result` message), detects the sentinel string in either the
error field or the synthetic `tool_result` body, and raises
`LLMError.providerUnavailable("agent SDK escaped tool intercept: <handler message>")`
so the runtime never silently observes a corrupted turn. This is a
diagnostic safety net, not a happy path.

### 4.5 Concurrency invariants

- `buildCodec` and `buildStubMcpServer` are pure and called per request.
  No shared state across `complete()` invocations.
- A fresh `mcpServers` map is built per request. The SDK consumes it via
  `options`; the provider never retains the server instance.
- `interceptor` state (`activeToolID`, accumulator buffers) is local to a
  single iteration.

## 5. Errors

| Condition | Behaviour |
|---|---|
| `tools[]` empty or undefined | No MCP registration; slice-1 happy path. |
| Sanitisation collision (`a.b` and `a_b`) | `buildCodec` throws `LLMError.invalidPrompt(...)` synchronously, before `query()` is called. |
| `inputSchema === null` for any tool | `validateTools` (slice 1) throws `LLMError.invalidPrompt`. Re-used unchanged. |
| Unknown JSON-Schema shape | Warn once, fall back to `z.any()`. Non-fatal. |
| Model emits unknown MCP name | `codec.decode` throws `LLMError.providerUnavailable(...)`. Invariant violation. |
| Stub handler invoked (race) | Sentinel detected → `LLMError.providerUnavailable("agent SDK escaped tool intercept: ...")`. |
| `iter.interrupt()` throws | Swallowed; warn. Best-effort, mirrors slice-3 stop semantics. |
| `usage === undefined` after interrupt | Fallback to `{ model, inputTokens: 0, outputTokens: 0, costCents: 0 }`; warn. Slice 5 will refine. |
| Stream handler throws during interception | Latches cause; first `stop()` rejects with `LLMError.streamClosed(cause)`. Re-uses slice-3 latching. |

## 6. Testing

### 6.1 New unit suites

- **`harness/test/llm/name-codec.test.ts`** — round-trip, collision,
  multi-dot, no-op, decode of both prefixed and bare names, unknown-name
  rejection, foreign-server prefix rejection.
- **`harness/test/llm/mcp-stub-server.test.ts`** — server name is
  `watchkeeper`, encoded tool names match codec output, every handler
  throws with the sentinel string, schema conversion handles basic JSON
  types and degrades to `z.any()` with one warning for unknown.
- **`harness/test/llm/interceptor.test.ts`** — driven by a fake async
  iterator. Cases: happy path (no tool use), single tool use, parallel
  tool use (multiple `tool_use` blocks in one assistant message), name
  decoding through codec, error-message propagation in assistant frame,
  stub-escaped race detection, usage fallback when interrupt precedes
  `result`, `interceptStream` event ordering with `message_stop` and
  `finishReason: "tool_use"`.

### 6.2 Extensions to existing suite

`harness/test/llm/claude-agent-provider.test.ts` gains ~4 integration
cases on the existing `queryImpl` DI seam:

- `complete()` with tools registers `options.mcpServers` keyed by
  `"watchkeeper"`.
- `complete()` returns `toolCalls[]` with runtime names (decoded).
- `stream()` with tools produces `tool_call_start` /
  `tool_call_delta` / `message_stop { finishReason: "tool_use" }` in the
  expected order.
- `complete()` without tools omits `mcpServers` entirely (parity with
  slice 1).

### 6.3 Out of scope for slice 4

- Cross-provider conformance parameterisation lives in slice 6
  (`harness/test/llm-conformance.test.ts`). Slice 4 only ensures the
  surface that slice 6 will exercise.
- Real `claude` CLI subscription smoke is manual, not CI (matches M5.7.b
  `SKIP_CLAUDE_TESTS` gating).
- Inbound `role=tool` test coverage belongs to M5.3.c.c.c.
- Multi-turn end-to-end loops across multiple `complete()` calls require
  inbound translation; deferred.

### 6.4 CI / lefthook expectations

- `pnpm -C harness test` green.
- `pnpm -C harness typecheck` green under
  `exactOptionalPropertyTypes: true` — build options via spread, never
  `obj.field = undefined`.
- `pnpm -C harness lint` green; any new eslint suppression carries an
  inline `--` justification (matches house style).
- Grep-invariant test in `core/pkg/secrets/invariant_test.go` is
  untouched — slice 4 does not introduce credential references.

## 7. Open questions

None requiring resolution before implementation. Possible refinements
that the operator may want to revisit during PR review:

- Whether `buildStubMcpServer` should also expose a "panic mode" where
  handlers `await` an abort signal instead of throwing, to remove even
  the diagnostic race path. Current judgement: throwing is simpler and
  the interceptor already detects the escape, so the abort-signal path
  is unnecessary complexity.
- Whether the MCP server name `"watchkeeper"` should be configurable.
  Current judgement: a constant keeps audit-log parsing and conformance
  expectations simple; the rename has no Phase-1 use case.
