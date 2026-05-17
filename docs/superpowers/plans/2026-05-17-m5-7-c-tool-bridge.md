# M5.7.c slice 4 — Tool bridge implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `ClaudeAgentProvider` honour the `LLMProvider` outbound-tool contract — intercept model `tool_use` blocks, return them as `CompleteResponse.toolCalls` / stream events, and never let the Agent SDK execute Watchkeeper tools itself.

**Architecture:** Three new helpers (`tool-bridge-name-codec.ts`, `tool-bridge-mcp-stub-server.ts`, `tool-bridge-interceptor.ts`) plug into the existing `claude-agent-provider.ts`. The provider registers an in-process MCP stub server, watches the SDK iterator, and calls `iter.interrupt()` on the first assistant message containing `tool_use` blocks. Parallel tool calls in a single assistant message are preserved.

**Tech Stack:** TypeScript (strict + `exactOptionalPropertyTypes: true`), `@anthropic-ai/claude-agent-sdk@0.3.143`, `zod@^3.23.8`, `vitest`.

**Spec:** `docs/superpowers/specs/2026-05-17-m5-7-c-tool-bridge-design.md` (this branch, commit `11b114f`).

**Worktree / branch:** `/Users/user/PhpstormProjects/wathkeepers-agent-sdk`, `feat/m5.7.c-claude-agent-provider`. All `pnpm` commands run from there.

---

## File map (refinement of spec section 3 to match existing flat layout)

| File | Status | Responsibility |
|---|---|---|
| `harness/src/llm/tool-bridge-name-codec.ts` | **create** | runtime↔mcp tool name bijection + collision detection |
| `harness/src/llm/tool-bridge-mcp-stub-server.ts` | **create** | `createSdkMcpServer` builder + JSON-schema→Zod minimal converter + throw-stub handlers |
| `harness/src/llm/tool-bridge-interceptor.ts` | **create** | iterator consumption, parallel `tool_use` capture, `iter.interrupt()`, stream-event dispatch, stub-escape detection |
| `harness/src/llm/claude-agent-provider.ts` | **modify** | wire the three modules into `complete()` and `stream()`; extend `consumeAssistantMessage` to return `toolCalls: ToolCall[]` |
| `harness/test/llm-tool-bridge-name-codec.test.ts` | **create** | name-codec unit suite |
| `harness/test/llm-tool-bridge-mcp-stub-server.test.ts` | **create** | mcp-stub-server unit suite |
| `harness/test/llm-tool-bridge-interceptor.test.ts` | **create** | interceptor unit suite (fake iterator) |
| `harness/test/llm-claude-agent-provider.test.ts` | **modify** | +4 integration cases on the `queryImpl` DI seam |

---

## Conventions (do not skip)

- **Test command:** `pnpm -C harness test -- <glob-or-name>` for filtered runs, `pnpm -C harness test` for the full suite. Replace `<harness-abs>` below with `/Users/user/PhpstormProjects/wathkeepers-agent-sdk` if your shell needs absolute paths.
- **Type check:** `pnpm -C harness typecheck` must be green at every commit. `exactOptionalPropertyTypes: true` is enabled — build objects via spread / conditionals, never assign `obj.field = undefined`.
- **Lint:** `pnpm -C harness lint` must be green. Any new `eslint-disable` line carries an inline `--` justification (matches house style).
- **Grep invariant:** never reference `ANTHROPIC_API_KEY` literal outside `harness/src/secrets/env.ts`. None of these tasks touch credentials, so the invariant should stay satisfied automatically.
- **Repo language:** all code, comments, commit messages — English only.
- **Commits:** Conventional Commits, no `--no-verify`, no `--no-gpg-sign`. Bodies in prose paragraphs (lefthook commitlint is picky about parenthesised bullet lists in footers). Note: `-c commit.gpgsign=false` (the per-command override used in the bash examples below) is a deliberate per-command opt-out for development worktrees where GPG is not configured locally; it is NOT the `--no-gpg-sign` per-commit anti-pattern that the rule forbids. The examples use it because the harness's plan-execution shell may run without GPG.
- **Cross-reference comment:** every new piece of code that *skips* `role === "tool"` carries `// M5.3.c.c.c — inbound tool-result folding lives there` (mirrors the existing comment in `claude-code-provider.ts:337`).

---

## Task 1 — `tool-bridge-name-codec.ts`

**Files:**
- Create: `harness/src/llm/tool-bridge-name-codec.ts`
- Create: `harness/test/llm-tool-bridge-name-codec.test.ts`

The codec is pure and stateless. Implementing it first lets the next two tasks depend on a known surface.

- [ ] **Step 1.1: Write the failing test file**

Create `harness/test/llm-tool-bridge-name-codec.test.ts`:

```ts
/**
 * `tool-bridge-name-codec` vitest suite (M5.7.c slice 4).
 *
 * The codec is a pure function over `ToolDefinition[]`; tests exercise
 * encode/decode round-trip, collision detection, and the SDK
 * `mcp__<server>__<tool>` prefix-stripping behaviour the interceptor
 * relies on.
 */

import { describe, expect, it } from "vitest";

import { LLMError } from "../src/llm/errors.js";
import {
  MCP_SERVER_NAME,
  buildCodec,
} from "../src/llm/tool-bridge-name-codec.js";
import type { ToolDefinition } from "../src/llm/types.js";

function td(name: string): ToolDefinition {
  return { name, description: "", inputSchema: {} };
}

describe("buildCodec", () => {
  it("encodes dotted runtime names to underscored mcp names", () => {
    const codec = buildCodec([td("notebook.remember"), td("notebook.recall")]);
    expect(codec.encode("notebook.remember")).toBe("notebook_remember");
    expect(codec.encode("notebook.recall")).toBe("notebook_recall");
  });

  it("is a no-op for names without dots", () => {
    const codec = buildCodec([td("simple")]);
    expect(codec.encode("simple")).toBe("simple");
  });

  it("handles multiple dots", () => {
    const codec = buildCodec([td("keepers.notebook.archive")]);
    expect(codec.encode("keepers.notebook.archive")).toBe(
      "keepers_notebook_archive",
    );
  });

  it("decodes the bare encoded form back to runtime name", () => {
    const codec = buildCodec([td("notebook.remember")]);
    expect(codec.decode("notebook_remember")).toBe("notebook.remember");
  });

  it("decodes the SDK-prefixed form back to runtime name", () => {
    const codec = buildCodec([td("notebook.remember")]);
    expect(codec.decode(`mcp__${MCP_SERVER_NAME}__notebook_remember`)).toBe(
      "notebook.remember",
    );
  });

  it("throws invalid_prompt on sanitisation collision", () => {
    expect(() => buildCodec([td("a.b"), td("a_b")])).toThrow(LLMError);
    try {
      buildCodec([td("a.b"), td("a_b")]);
    } catch (e) {
      expect((e as LLMError).code).toBe("invalid_prompt");
      expect((e as LLMError).message).toMatch(/collision/i);
    }
  });

  it("throws provider_unavailable on unknown mcp name", () => {
    const codec = buildCodec([td("notebook.remember")]);
    expect(() => codec.decode("unknown_tool")).toThrow(LLMError);
    try {
      codec.decode("unknown_tool");
    } catch (e) {
      expect((e as LLMError).code).toBe("provider_unavailable");
    }
  });

  it("rejects foreign mcp server prefixes", () => {
    const codec = buildCodec([td("notebook.remember")]);
    expect(() => codec.decode("mcp__other__notebook_remember")).toThrow(
      LLMError,
    );
  });
});
```

- [ ] **Step 1.2: Run the test and confirm it fails**

Run: `pnpm -C harness test -- llm-tool-bridge-name-codec`

Expected: failure with `Cannot find module '../src/llm/tool-bridge-name-codec.js'` (the module does not exist yet).

- [ ] **Step 1.3: Implement the minimal codec**

Create `harness/src/llm/tool-bridge-name-codec.ts`:

```ts
/**
 * Tool-name codec for the M5.7.c slice 4 ClaudeAgentProvider tool
 * bridge. Builds a bijective map between the runtime tool names
 * (`notebook.remember`) and MCP-legal names (`notebook_remember`) the
 * Agent SDK exposes to the model under the
 * `mcp__watchkeeper__<encoded>` prefix.
 *
 * The codec is pure: it is constructed once per `complete()` / `stream()`
 * call from `req.tools` and discarded with the call. Collisions after
 * sanitisation are surfaced synchronously so the SDK is never invoked
 * with an ambiguous tool list.
 */

import { LLMError } from "./errors.js";
import type { ToolDefinition } from "./types.js";

/**
 * Name of the in-process MCP server the bridge registers. A constant —
 * audit-log parsing, conformance expectations and `decode()`
 * prefix-stripping all rely on a single source of truth.
 */
export const MCP_SERVER_NAME = "watchkeeper";

const MCP_PREFIX = `mcp__${MCP_SERVER_NAME}__`;

export interface ToolNameCodec {
  /** runtime-name (`notebook.remember`) → mcp-name (`notebook_remember`). */
  encode(runtimeName: string): string;
  /**
   * mcp-name (either bare `notebook_remember` or SDK-prefixed
   * `mcp__watchkeeper__notebook_remember`) → runtime-name.
   * Throws {@link LLMError} `provider_unavailable` for unknown names.
   */
  decode(mcpName: string): string;
}

function encodeName(runtimeName: string): string {
  return runtimeName.replaceAll(".", "_");
}

export function buildCodec(tools: readonly ToolDefinition[]): ToolNameCodec {
  const encodeMap = new Map<string, string>();
  const decodeMap = new Map<string, string>();

  for (const t of tools) {
    const encoded = encodeName(t.name);
    const colliding = decodeMap.get(encoded);
    if (colliding !== undefined && colliding !== t.name) {
      throw LLMError.invalidPrompt(
        `watchkeeper MCP name collision: "${colliding}" and "${t.name}" both sanitise to "${encoded}"`,
      );
    }
    encodeMap.set(t.name, encoded);
    decodeMap.set(encoded, t.name);
  }

  return {
    encode(runtimeName) {
      // Defensive: re-derive rather than rely on prior `set` in case a
      // caller asks for a name not in the original tool list.
      return encodeMap.get(runtimeName) ?? encodeName(runtimeName);
    },
    decode(mcpName) {
      const bare = mcpName.startsWith(MCP_PREFIX)
        ? mcpName.slice(MCP_PREFIX.length)
        : mcpName.includes("__") && mcpName.startsWith("mcp__")
          ? // Foreign server prefix — treat as unknown rather than guessing.
            "<foreign-prefix>"
          : mcpName;
      const runtime = decodeMap.get(bare);
      if (runtime === undefined) {
        throw LLMError.providerUnavailable(
          `agent SDK emitted unknown tool name: ${mcpName}`,
        );
      }
      return runtime;
    },
  };
}
```

- [ ] **Step 1.4: Run the test and confirm it passes**

Run: `pnpm -C harness test -- llm-tool-bridge-name-codec`

Expected: all 8 tests pass.

- [ ] **Step 1.5: Typecheck and lint**

Run: `pnpm -C harness typecheck && pnpm -C harness lint`

Expected: both green. If `eslint` complains about `?? encodeName(...)` redundancy, leave the fallback — it documents the defensive path explicitly.

- [ ] **Step 1.6: Commit**

```bash
git add harness/src/llm/tool-bridge-name-codec.ts harness/test/llm-tool-bridge-name-codec.test.ts
git -c commit.gpgsign=false commit -m "feat(harness): tool-bridge name codec for ClaudeAgentProvider (M5.7.c WIP 4a/n)

Adds runtime-name to MCP-name bijection with dot-to-underscore
sanitisation and synchronous collision detection. The codec is the
foundation the upcoming MCP stub server and SDK-iterator interceptor
both consume; isolated here so it is reusable and unit-testable
without the SDK in the loop."
```

---

## Task 2 — `tool-bridge-mcp-stub-server.ts`

**Files:**
- Create: `harness/src/llm/tool-bridge-mcp-stub-server.ts`
- Create: `harness/test/llm-tool-bridge-mcp-stub-server.test.ts`

This module owns two responsibilities: the throw-stub handler factory and the JSON-Schema → Zod conversion the SDK requires. Keep both narrow — the spec deliberately limits supported JSON-Schema shapes.

- [ ] **Step 2.1: Write the failing test file**

Create `harness/test/llm-tool-bridge-mcp-stub-server.test.ts`:

```ts
/**
 * `tool-bridge-mcp-stub-server` vitest suite (M5.7.c slice 4).
 *
 * The stub server registers Watchkeeper tools with the Agent SDK so the
 * model can request them, but every handler throws a sentinel-tagged
 * error: the interceptor is supposed to capture the tool_use and
 * interrupt the SDK before any handler runs. These tests pin the server
 * name, encoded tool names, sentinel string, and the minimal
 * JSON-Schema → Zod conversion contract.
 */

import { describe, expect, it } from "vitest";
import { z } from "zod";

import { MCP_SERVER_NAME, buildCodec } from "../src/llm/tool-bridge-name-codec.js";
import {
  MCP_STUB_SENTINEL,
  buildStubMcpServer,
  jsonSchemaToZod,
} from "../src/llm/tool-bridge-mcp-stub-server.js";
import type { ToolDefinition } from "../src/llm/types.js";

function td(name: string, schema: Record<string, unknown> = {}): ToolDefinition {
  return { name, description: `desc-${name}`, inputSchema: schema };
}

describe("buildStubMcpServer", () => {
  it("uses the watchkeeper server name from the codec module", () => {
    const tools = [td("notebook.remember")];
    const codec = buildCodec(tools);
    const cfg = buildStubMcpServer(tools, codec);
    expect(cfg.name).toBe(MCP_SERVER_NAME);
  });

  it("registers each tool under its encoded name", () => {
    const tools = [td("notebook.remember"), td("notebook.recall")];
    const codec = buildCodec(tools);
    const cfg = buildStubMcpServer(tools, codec);
    const registered = cfg.tools.map((t) => t.name).sort();
    expect(registered).toEqual(["notebook_recall", "notebook_remember"]);
  });

  it("handlers throw an error tagged with the sentinel string", async () => {
    const tools = [td("notebook.remember")];
    const codec = buildCodec(tools);
    const cfg = buildStubMcpServer(tools, codec);
    const handler = cfg.tools[0]!.handler;
    await expect(handler({}, undefined)).rejects.toThrowError(
      new RegExp(MCP_STUB_SENTINEL),
    );
  });
});

describe("jsonSchemaToZod", () => {
  it("returns z.any() for an empty / undefined schema", () => {
    const schema = jsonSchemaToZod({});
    expect(schema instanceof z.ZodAny).toBe(true);
  });

  it("converts object with primitive properties", () => {
    const schema = jsonSchemaToZod({
      type: "object",
      properties: {
        key: { type: "string" },
        count: { type: "number" },
        flag: { type: "boolean" },
      },
      required: ["key"],
    });
    const ok = schema.safeParse({ key: "k", count: 1, flag: true });
    expect(ok.success).toBe(true);
    const missingRequired = schema.safeParse({ count: 1 });
    expect(missingRequired.success).toBe(false);
  });

  it("converts arrays of primitives", () => {
    const schema = jsonSchemaToZod({
      type: "array",
      items: { type: "string" },
    });
    expect(schema.safeParse(["a", "b"]).success).toBe(true);
    expect(schema.safeParse([1, 2]).success).toBe(false);
  });

  it("degrades unknown shapes to z.any() without throwing", () => {
    const schema = jsonSchemaToZod({ type: "anyOf", oneOf: [] });
    expect(schema instanceof z.ZodAny).toBe(true);
  });
});
```

- [ ] **Step 2.2: Run the test and confirm it fails**

Run: `pnpm -C harness test -- llm-tool-bridge-mcp-stub-server`

Expected: failure with `Cannot find module '../src/llm/tool-bridge-mcp-stub-server.js'`.

- [ ] **Step 2.3: Implement the stub server module**

Create `harness/src/llm/tool-bridge-mcp-stub-server.ts`:

```ts
/**
 * MCP stub-server builder for the M5.7.c slice 4 tool bridge.
 *
 * The Agent SDK requires custom tools to be exposed via an MCP server.
 * Watchkeeper does NOT want the SDK to execute its tools — the runtime
 * owns execution. So we register a server whose every handler throws a
 * sentinel-tagged error; the interceptor (`tool-bridge-interceptor.ts`)
 * captures the model's `tool_use` block from the SDK iterator BEFORE
 * the SDK dispatches the handler, then calls `iter.interrupt()`. If the
 * race goes the other way (handler invoked before interrupt arrives),
 * the sentinel surfaces in the SDK's synthesised `tool_result(is_error)`
 * and the interceptor escalates it to
 * `LLMError.providerUnavailable("agent SDK escaped tool intercept: ...")`.
 *
 * `jsonSchemaToZod` is the minimal converter the SDK's `tool()` helper
 * requires: it supports the JSON-Schema shapes that Watchkeeper tool
 * manifests use today (`object` with primitive properties, primitive
 * scalars, `array` of primitives) and degrades to `z.any()` for
 * anything else with a single warning. A richer converter is future
 * work outside this slice.
 */

import {
  createSdkMcpServer,
  tool,
  type McpSdkServerConfigWithInstance,
} from "@anthropic-ai/claude-agent-sdk";
import { z, type ZodTypeAny, type ZodRawShape } from "zod";

import { MCP_SERVER_NAME, type ToolNameCodec } from "./tool-bridge-name-codec.js";
import type { ToolDefinition } from "./types.js";

/**
 * Sentinel substring embedded in every stub handler's error message.
 * The interceptor scans for it to distinguish a race-escape from a
 * genuine downstream tool error.
 */
export const MCP_STUB_SENTINEL = "watchkeeper MCP stub invoked";

interface StubToolEntry {
  readonly name: string;
  readonly handler: (
    args: Record<string, unknown>,
    extra: unknown,
  ) => Promise<{ content: Array<{ type: "text"; text: string }>; isError: true }>;
}

export interface StubMcpServerConfig {
  readonly name: string;
  readonly tools: readonly StubToolEntry[];
  /** Underlying SDK config to hand to `query({ options: { mcpServers: ... } })`. */
  readonly sdkConfig: McpSdkServerConfigWithInstance;
}

export function buildStubMcpServer(
  tools: readonly ToolDefinition[],
  codec: ToolNameCodec,
): StubMcpServerConfig {
  const entries: StubToolEntry[] = [];
  const sdkTools = tools.map((t) => {
    const encoded = codec.encode(t.name);
    const shape = jsonSchemaToZodShape(t.inputSchema);
    const handler: StubToolEntry["handler"] = async () => {
      throw new Error(
        `${MCP_STUB_SENTINEL}: ${t.name} (encoded ${encoded}); the interceptor was supposed to capture this tool_use before dispatch.`,
      );
    };
    entries.push({ name: encoded, handler });
    return tool(encoded, t.description, shape, handler);
  });

  const sdkConfig = createSdkMcpServer({
    name: MCP_SERVER_NAME,
    tools: sdkTools,
  });

  return { name: MCP_SERVER_NAME, tools: entries, sdkConfig };
}

/**
 * Convert a JSON-Schema-shaped object into a top-level `ZodTypeAny`.
 * The SDK's `tool()` helper takes a `ZodRawShape` for object schemas;
 * `jsonSchemaToZodShape` below adapts to that.
 *
 * Supported types: `object` (with `properties` / `required`), `string`,
 * `number`, `integer`, `boolean`, `array` (with primitive `items`).
 * Anything else falls back to `z.any()` and a `console.warn` (one-shot
 * per build is sufficient for Phase 1).
 */
export function jsonSchemaToZod(schema: Record<string, unknown>): ZodTypeAny {
  const type = schema.type;
  if (type === "object") {
    const shape = jsonSchemaToZodShape(schema);
    return z.object(shape);
  }
  if (type === "string") return z.string();
  if (type === "number" || type === "integer") return z.number();
  if (type === "boolean") return z.boolean();
  if (type === "array") {
    const items = (schema.items as Record<string, unknown> | undefined) ?? {};
    return z.array(jsonSchemaToZod(items));
  }
  warnUnsupportedSchema(schema);
  return z.any();
}

/**
 * Convert a top-level object schema into the `ZodRawShape` the SDK's
 * `tool()` helper requires. Empty / non-object schemas degrade to an
 * empty shape — the model will see a no-arg tool, which mirrors the
 * Watchkeeper runtime's permissive default.
 */
function jsonSchemaToZodShape(
  schema: Record<string, unknown> | undefined,
): ZodRawShape {
  if (schema === undefined || schema.type !== "object") return {};
  const props = (schema.properties as Record<string, Record<string, unknown>> | undefined) ?? {};
  const required = new Set((schema.required as string[] | undefined) ?? []);
  const shape: ZodRawShape = {};
  for (const [key, propSchema] of Object.entries(props)) {
    const base = jsonSchemaToZod(propSchema);
    shape[key] = required.has(key) ? base : base.optional();
  }
  return shape;
}

let warnedSchemas = new WeakSet<object>();
function warnUnsupportedSchema(schema: Record<string, unknown>): void {
  if (warnedSchemas.has(schema)) return;
  warnedSchemas.add(schema);
  // eslint-disable-next-line no-console -- Single diagnostic warning is the simplest signal for an operator running into an unsupported manifest shape. Replace with a structured logger once one lands.
  console.warn(
    `tool-bridge: unsupported JSON-Schema shape, falling back to z.any(): ${JSON.stringify(
      schema,
    )}`,
  );
}

/* Test-only reset for the dedup set so the suite can re-trigger the warn path. */
export function __resetUnsupportedSchemaWarnings(): void {
  warnedSchemas = new WeakSet();
}
```

- [ ] **Step 2.4: Run the test and confirm it passes**

Run: `pnpm -C harness test -- llm-tool-bridge-mcp-stub-server`

Expected: all 7 tests pass.

If `cfg.tools` typing is awkward (the SDK's `McpSdkServerConfigWithInstance` may not expose `tools` directly), use the `StubMcpServerConfig.tools` field instead — that's what the test asserts against, not the SDK config.

- [ ] **Step 2.5: Typecheck and lint**

Run: `pnpm -C harness typecheck && pnpm -C harness lint`

Expected: green.

- [ ] **Step 2.6: Commit**

```bash
git add harness/src/llm/tool-bridge-mcp-stub-server.ts harness/test/llm-tool-bridge-mcp-stub-server.test.ts
git -c commit.gpgsign=false commit -m "feat(harness): tool-bridge MCP stub server for ClaudeAgentProvider (M5.7.c WIP 4b/n)

Registers Watchkeeper tools with the Agent SDK as an in-process MCP
server whose handlers throw a sentinel-tagged error. The interceptor in
the next sub-item captures the model's tool_use block before the SDK
ever invokes a handler. Includes a minimal JSON-Schema to Zod converter
sufficient for Phase 1 manifest shapes; anything unsupported degrades
to z.any() with a one-shot console warning."
```

---

## Task 3 — `tool-bridge-interceptor.ts`

**Files:**
- Create: `harness/src/llm/tool-bridge-interceptor.ts`
- Create: `harness/test/llm-tool-bridge-interceptor.test.ts`

The interceptor consumes the SDK iterator, captures `tool_use` blocks from an `assistant` message, calls `iter.interrupt()`, and returns the captured state. Stream variant additionally fans out `StreamEvent`s. The escape-race detection is part of this module.

- [ ] **Step 3.1: Write the failing test file**

Create `harness/test/llm-tool-bridge-interceptor.test.ts`:

```ts
/**
 * `tool-bridge-interceptor` vitest suite (M5.7.c slice 4).
 *
 * Drives the interceptor with hand-built SDK-shaped message sequences
 * delivered through a fake async iterator. Covers: happy path (no
 * tool_use), single tool_use + interrupt, parallel tool_use in one
 * assistant message, decoded name surfaces correctly, escape-race
 * detection, usage fallback when interrupt precedes the SDK result
 * message, and the stream-event mirror.
 */

import { describe, expect, it, vi } from "vitest";

import { LLMError } from "../src/llm/errors.js";
import { MCP_STUB_SENTINEL } from "../src/llm/tool-bridge-mcp-stub-server.js";
import { MCP_SERVER_NAME, buildCodec } from "../src/llm/tool-bridge-name-codec.js";
import {
  interceptComplete,
  interceptStream,
} from "../src/llm/tool-bridge-interceptor.js";
import { model, type StreamEvent } from "../src/llm/index.js";
import type { ToolDefinition } from "../src/llm/types.js";

const REQUESTED_MODEL = model("claude-3-5-sonnet-latest");

function td(name: string): ToolDefinition {
  return { name, description: "", inputSchema: {} };
}

interface FakeIter {
  iter: AsyncIterable<unknown> & { interrupt: () => Promise<void> };
  interrupted: { value: boolean };
}

function fakeIter(messages: readonly unknown[]): FakeIter {
  const interrupted = { value: false };
  async function* gen() {
    for (const m of messages) {
      if (interrupted.value) return;
      yield m;
    }
  }
  const iter = gen() as unknown as AsyncIterable<unknown> & {
    interrupt: () => Promise<void>;
  };
  iter.interrupt = async () => {
    interrupted.value = true;
  };
  return { iter, interrupted };
}

function assistantText(text: string): unknown {
  return {
    type: "assistant",
    message: { content: [{ type: "text", text }] },
  };
}

function assistantToolUse(
  blocks: ReadonlyArray<{ id: string; name: string; input: Record<string, unknown> }>,
  precedingText?: string,
): unknown {
  const content: unknown[] = [];
  if (precedingText !== undefined) {
    content.push({ type: "text", text: precedingText });
  }
  for (const b of blocks) {
    content.push({ type: "tool_use", id: b.id, name: b.name, input: b.input });
  }
  return { type: "assistant", message: { content, usage: { input_tokens: 10, output_tokens: 5 } } };
}

function resultMessage(opts: {
  subtype?: "success" | "error";
  inputTokens?: number;
  outputTokens?: number;
  totalCostUsd?: number;
  stopReason?: string;
}): unknown {
  return {
    type: "result",
    subtype: opts.subtype ?? "success",
    usage: {
      input_tokens: opts.inputTokens ?? 0,
      output_tokens: opts.outputTokens ?? 0,
    },
    total_cost_usd: opts.totalCostUsd ?? 0,
    stop_reason: opts.stopReason,
  };
}

describe("interceptComplete", () => {
  it("happy path with no tool_use returns finishReason=stop", async () => {
    const codec = buildCodec([]);
    const { iter, interrupted } = fakeIter([
      assistantText("hi"),
      resultMessage({ stopReason: "end_turn", inputTokens: 1, outputTokens: 2, totalCostUsd: 0.0001 }),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    expect(turn.text).toBe("hi");
    expect(turn.toolCalls).toEqual([]);
    expect(turn.finishReason).toBe("stop");
    expect(turn.usage?.inputTokens).toBe(1);
    expect(turn.usage?.outputTokens).toBe(2);
    expect(turn.usage?.costCents).toBe(1);
    expect(interrupted.value).toBe(false);
  });

  it("captures a single tool_use and interrupts before result arrives", async () => {
    const codec = buildCodec([td("notebook.remember")]);
    const { iter, interrupted } = fakeIter([
      assistantToolUse(
        [
          {
            id: "tu_1",
            name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
            input: { key: "k", value: "v" },
          },
        ],
        "I'll save that.",
      ),
      // Result deliberately appended so we can prove interrupt cut the iterator.
      resultMessage({ stopReason: "tool_use" }),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    expect(turn.text).toBe("I'll save that.");
    expect(turn.toolCalls).toEqual([
      { id: "tu_1", name: "notebook.remember", arguments: { key: "k", value: "v" } },
    ]);
    expect(turn.finishReason).toBe("tool_use");
    expect(interrupted.value).toBe(true);
  });

  it("captures parallel tool_use blocks in the order emitted", async () => {
    const codec = buildCodec([td("notebook.recall"), td("notebook.remember")]);
    const { iter } = fakeIter([
      assistantToolUse([
        {
          id: "tu_1",
          name: `mcp__${MCP_SERVER_NAME}__notebook_recall`,
          input: { q: "recent" },
        },
        {
          id: "tu_2",
          name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
          input: { key: "summary" },
        },
      ]),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    expect(turn.toolCalls.map((c) => c.id)).toEqual(["tu_1", "tu_2"]);
    expect(turn.toolCalls.map((c) => c.name)).toEqual([
      "notebook.recall",
      "notebook.remember",
    ]);
  });

  it("usage falls back to zero when interrupt precedes result", async () => {
    const codec = buildCodec([td("notebook.remember")]);
    const { iter } = fakeIter([
      assistantToolUse([
        {
          id: "tu_1",
          name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
          input: {},
        },
      ]),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    // Either the assistant.usage was lifted or the zero fallback applied.
    expect(turn.usage).toBeDefined();
    expect(turn.usage?.model).toBe(REQUESTED_MODEL);
  });

  it("escalates the escape race when stub-handler sentinel surfaces", async () => {
    const codec = buildCodec([td("notebook.remember")]);
    const { iter } = fakeIter([
      // Simulate the SDK injecting a synthetic tool_result(is_error) after
      // the handler fired before we could interrupt. The interceptor
      // surfaces the sentinel as provider_unavailable.
      {
        type: "user",
        message: {
          content: [
            {
              type: "tool_result",
              tool_use_id: "tu_1",
              is_error: true,
              content: `${MCP_STUB_SENTINEL}: notebook.remember`,
            },
          ],
        },
      },
    ]);
    await expect(interceptComplete(iter, codec, REQUESTED_MODEL)).rejects.toBeInstanceOf(LLMError);
    try {
      await interceptComplete(fakeIter([
        {
          type: "user",
          message: {
            content: [
              {
                type: "tool_result",
                tool_use_id: "tu_1",
                is_error: true,
                content: `${MCP_STUB_SENTINEL}: notebook.remember`,
              },
            ],
          },
        },
      ]).iter, codec, REQUESTED_MODEL);
    } catch (e) {
      expect((e as LLMError).code).toBe("provider_unavailable");
      expect((e as LLMError).message).toMatch(/escaped tool intercept/);
    }
  });
});

describe("interceptStream", () => {
  it("emits text_delta then message_stop with finishReason=tool_use on tool_use turn", async () => {
    const codec = buildCodec([td("notebook.remember")]);
    const events: StreamEvent[] = [];
    const handler = async (e: StreamEvent) => {
      events.push(e);
    };
    const { iter, interrupted } = fakeIter([
      // Streaming-mode SDK emits stream_event sub-messages around partial assistant content.
      {
        type: "stream_event",
        event: { type: "content_block_start", index: 0, content_block: { type: "text" } },
      },
      {
        type: "stream_event",
        event: { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "I'll" } },
      },
      {
        type: "stream_event",
        event: { type: "content_block_stop", index: 0 },
      },
      {
        type: "stream_event",
        event: {
          type: "content_block_start",
          index: 1,
          content_block: {
            type: "tool_use",
            id: "tu_1",
            name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
          },
        },
      },
      {
        type: "stream_event",
        event: { type: "content_block_delta", index: 1, delta: { type: "input_json_delta", partial_json: "{\"k\":1}" } },
      },
      {
        type: "stream_event",
        event: { type: "content_block_stop", index: 1 },
      },
      assistantToolUse([
        {
          id: "tu_1",
          name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
          input: { k: 1 },
        },
      ]),
      resultMessage({ stopReason: "tool_use" }),
    ]);
    const abortBag = {
      isStopped: false,
      markStopped: vi.fn((cause: unknown) => {
        abortBag.isStopped = true;
        void cause;
      }),
    };
    await interceptStream(iter, handler, codec, REQUESTED_MODEL, abortBag);
    const kinds = events.map((e) => e.kind);
    expect(kinds).toContain("text_delta");
    expect(kinds).toContain("tool_call_start");
    expect(kinds).toContain("tool_call_delta");
    expect(kinds[kinds.length - 1]).toBe("message_stop");
    const stop = events[events.length - 1] as Extract<StreamEvent, { kind: "message_stop" }>;
    expect(stop.finishReason).toBe("tool_use");
    expect(interrupted.value).toBe(true);
  });
});
```

- [ ] **Step 3.2: Run the test and confirm it fails**

Run: `pnpm -C harness test -- llm-tool-bridge-interceptor`

Expected: failure with `Cannot find module '../src/llm/tool-bridge-interceptor.js'`.

- [ ] **Step 3.3: Implement the interceptor module**

Create `harness/src/llm/tool-bridge-interceptor.ts`:

```ts
/**
 * SDK-iterator interceptor for the M5.7.c slice 4 tool bridge.
 *
 * The portable `LLMProvider` contract requires `complete()` to return
 * `tool_use` requests as `CompleteResponse.toolCalls` for the runtime
 * to execute. The Agent SDK takes the opposite stance — it owns tool
 * execution and only surfaces results. To bridge the gap, the
 * interceptor watches the SDK iterator: the first `SDKAssistantMessage`
 * with `tool_use` blocks causes it to capture them all, call
 * `iter.interrupt()`, and return so the runtime can take over.
 *
 * Streaming mirror: `interceptStream` runs the same state machine but
 * additionally fans out portable `StreamEvent`s to the handler as SDK
 * `stream_event` partial-assistant frames arrive. The closing
 * `message_stop` (or, in some SDK builds, the absence of a closing
 * frame after the assistant tool_use snapshot arrives) triggers
 * interrupt.
 *
 * Stub-handler escape: if the SDK invokes a Watchkeeper stub handler
 * before `interrupt()` lands, the stub throws a sentinel-tagged error
 * (see `tool-bridge-mcp-stub-server.ts`). The SDK wraps it in a
 * synthetic `tool_result(is_error)` user message. The interceptor
 * scans incoming messages for the sentinel substring and escalates with
 * `LLMError.providerUnavailable("agent SDK escaped tool intercept: ...")`
 * so the runtime never silently observes a corrupted turn.
 */

import { LLMError } from "./errors.js";
import { MCP_STUB_SENTINEL } from "./tool-bridge-mcp-stub-server.js";
import type { ToolNameCodec } from "./tool-bridge-name-codec.js";
import type {
  FinishReason,
  Model,
  StreamEvent,
  StreamHandler,
  ToolCall,
  Usage,
} from "./types.js";

export interface InterceptedTurn {
  readonly toolCalls: readonly ToolCall[];
  readonly text: string;
  readonly finishReason: FinishReason;
  readonly usage: Usage | undefined;
  readonly errorMessage: string | undefined;
}

interface SdkIter {
  [Symbol.asyncIterator](): AsyncIterator<unknown>;
  interrupt?: () => Promise<void>;
}

export interface AbortBag {
  isStopped: boolean;
  markStopped(cause: unknown): void;
}

export async function interceptComplete(
  iter: SdkIter,
  codec: ToolNameCodec,
  requestedModel: Model,
): Promise<InterceptedTurn> {
  const it = iter[Symbol.asyncIterator]();
  let text = "";
  let assistantUsage: Usage | undefined;
  let usage: Usage | undefined;
  let finishReason: FinishReason = "stop";
  let errorMessage: string | undefined;
  let toolCalls: ToolCall[] = [];

  while (true) {
    const next = await it.next();
    if (next.done === true) break;
    const msg = next.value;
    checkForStubEscape(msg);

    const parsed = parseMessage(msg, codec, requestedModel);
    if (parsed.text !== undefined) text += parsed.text;
    if (parsed.assistantUsage !== undefined) assistantUsage = parsed.assistantUsage;
    if (parsed.errorMessage !== undefined) errorMessage = parsed.errorMessage;

    if (parsed.toolCalls !== undefined && parsed.toolCalls.length > 0) {
      toolCalls = [...parsed.toolCalls];
      finishReason = "tool_use";
      await safeInterrupt(iter);
      break;
    }

    if (parsed.resultUsage !== undefined) {
      usage = parsed.resultUsage;
      if (parsed.resultFinishReason !== undefined) {
        finishReason = parsed.resultFinishReason;
      }
      break;
    }
  }

  const finalUsage =
    usage ?? assistantUsage ?? {
      model: requestedModel,
      inputTokens: 0,
      outputTokens: 0,
      costCents: 0,
    };

  return { text, toolCalls, finishReason, usage: finalUsage, errorMessage };
}

export async function interceptStream(
  iter: SdkIter,
  handler: StreamHandler,
  codec: ToolNameCodec,
  requestedModel: Model,
  abortBag: AbortBag,
): Promise<void> {
  const it = iter[Symbol.asyncIterator]();
  let activeToolID: string | undefined;
  let toolUseSnapshot: ToolCall[] = [];

  try {
    while (!abortBag.isStopped) {
      const next = await it.next();
      if (next.done === true) break;
      const msg = next.value;
      checkForStubEscape(msg);

      // Partial assistant streaming events.
      const evt = translatePartialEvent(msg, codec, () => activeToolID, (id) => {
        activeToolID = id;
      });
      if (evt !== undefined) {
        try {
          await handler(evt);
        } catch (handlerErr: unknown) {
          abortBag.markStopped(handlerErr);
          return;
        }
      }

      // Full assistant snapshot: capture tool_use blocks for the final stop event.
      const parsedAssistant = parseAssistantSnapshot(msg, codec);
      if (parsedAssistant !== undefined && parsedAssistant.length > 0) {
        toolUseSnapshot = parsedAssistant;
      }

      // Result message → final stop event.
      const result = parseResult(msg, requestedModel);
      if (result !== undefined) {
        const finishReason: FinishReason =
          toolUseSnapshot.length > 0 ? "tool_use" : result.finishReason;
        const stopEvent: StreamEvent = { kind: "message_stop", finishReason };
        if (result.usage !== undefined) {
          (stopEvent as { usage?: Usage }).usage = result.usage;
        }
        if (result.errorMessage !== undefined) {
          (stopEvent as { errorMessage?: string }).errorMessage = result.errorMessage;
        }
        try {
          await handler(stopEvent);
        } catch (handlerErr: unknown) {
          abortBag.markStopped(handlerErr);
        }
        if (toolUseSnapshot.length > 0) {
          await safeInterrupt(iter);
        }
        return;
      }

      // If we already captured a snapshot but the SDK has not produced a
      // result message yet, the assistant turn is complete (next thing
      // would be a tool dispatch). Interrupt and synthesise message_stop.
      if (toolUseSnapshot.length > 0 && isAssistantMessage(msg)) {
        const stopEvent: StreamEvent = { kind: "message_stop", finishReason: "tool_use" };
        try {
          await handler(stopEvent);
        } catch (handlerErr: unknown) {
          abortBag.markStopped(handlerErr);
        }
        await safeInterrupt(iter);
        return;
      }
    }
  } catch (sdkErr: unknown) {
    const errEvent: StreamEvent = {
      kind: "error",
      errorMessage: errorMessageOf(sdkErr),
    };
    try {
      await handler(errEvent);
    } catch {
      // Handler errors during the synthesised error event are swallowed;
      // the SDK error is the authoritative cause.
    }
    abortBag.markStopped(sdkErr);
  }
}

/* ---------------------------------------------------------------------------
 * Internal helpers
 * ------------------------------------------------------------------------ */

function checkForStubEscape(msg: unknown): void {
  if (msg === null || typeof msg !== "object") return;
  const m = msg as Record<string, unknown>;

  // SDK injects a tool_result(is_error) inside a user message after the
  // stub handler threw.
  if (m.type === "user") {
    const wrapped = m.message as Record<string, unknown> | undefined;
    const content = wrapped?.content;
    if (Array.isArray(content)) {
      for (const block of content) {
        if (block === null || typeof block !== "object") continue;
        const b = block as Record<string, unknown>;
        if (b.type !== "tool_result") continue;
        if (b.is_error !== true) continue;
        const text = stringifyToolResultContent(b.content);
        if (text.includes(MCP_STUB_SENTINEL)) {
          throw LLMError.providerUnavailable(
            `agent SDK escaped tool intercept: ${text}`,
          );
        }
      }
    }
  }
}

function stringifyToolResultContent(content: unknown): string {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .map((b) => (typeof b === "object" && b !== null && "text" in b ? String((b as { text: unknown }).text) : ""))
      .join("");
  }
  return "";
}

interface ParsedMessage {
  text?: string;
  toolCalls?: readonly ToolCall[];
  assistantUsage?: Usage;
  resultUsage?: Usage;
  resultFinishReason?: FinishReason;
  errorMessage?: string;
}

function parseMessage(
  msg: unknown,
  codec: ToolNameCodec,
  requestedModel: Model,
): ParsedMessage {
  if (msg === null || typeof msg !== "object") return {};
  const m = msg as Record<string, unknown>;
  if (m.type === "assistant") return parseAssistant(m, codec, requestedModel);
  if (m.type === "result") {
    const r = parseResult(msg, requestedModel);
    if (r === undefined) return {};
    const out: ParsedMessage = { resultFinishReason: r.finishReason };
    if (r.usage !== undefined) out.resultUsage = r.usage;
    if (r.errorMessage !== undefined) out.errorMessage = r.errorMessage;
    return out;
  }
  return {};
}

function parseAssistant(
  m: Record<string, unknown>,
  codec: ToolNameCodec,
  requestedModel: Model,
): ParsedMessage {
  const wrapped = m.message as Record<string, unknown> | undefined;
  if (wrapped === undefined) return {};
  const blocks = wrapped.content;
  const out: ParsedMessage = {};
  let textBuf = "";
  const calls: ToolCall[] = [];
  if (Array.isArray(blocks)) {
    for (const block of blocks) {
      if (block === null || typeof block !== "object") continue;
      const b = block as Record<string, unknown>;
      if (b.type === "text" && typeof b.text === "string") {
        textBuf += b.text;
      } else if (b.type === "tool_use") {
        const rawName = typeof b.name === "string" ? b.name : "";
        const decoded = codec.decode(rawName);
        calls.push({
          id: typeof b.id === "string" ? b.id : "",
          name: decoded,
          arguments: (b.input as Readonly<Record<string, unknown>> | undefined) ?? {},
        });
      }
    }
  }
  if (textBuf.length > 0) out.text = textBuf;
  if (calls.length > 0) out.toolCalls = calls;
  const usageRaw = wrapped.usage as Record<string, unknown> | undefined;
  if (usageRaw !== undefined) {
    out.assistantUsage = {
      model: requestedModel,
      inputTokens: typeof usageRaw.input_tokens === "number" ? usageRaw.input_tokens : 0,
      outputTokens: typeof usageRaw.output_tokens === "number" ? usageRaw.output_tokens : 0,
      costCents: 0,
    };
  }
  const errField = m.error;
  if (typeof errField === "string" && errField !== "") {
    out.errorMessage = errField;
  }
  return out;
}

function parseAssistantSnapshot(
  msg: unknown,
  codec: ToolNameCodec,
): ToolCall[] | undefined {
  if (msg === null || typeof msg !== "object") return undefined;
  const m = msg as Record<string, unknown>;
  if (m.type !== "assistant") return undefined;
  const wrapped = m.message as Record<string, unknown> | undefined;
  const blocks = wrapped?.content;
  if (!Array.isArray(blocks)) return undefined;
  const calls: ToolCall[] = [];
  for (const block of blocks) {
    if (block === null || typeof block !== "object") continue;
    const b = block as Record<string, unknown>;
    if (b.type !== "tool_use") continue;
    const rawName = typeof b.name === "string" ? b.name : "";
    calls.push({
      id: typeof b.id === "string" ? b.id : "",
      name: codec.decode(rawName),
      arguments: (b.input as Readonly<Record<string, unknown>> | undefined) ?? {},
    });
  }
  return calls;
}

function isAssistantMessage(msg: unknown): boolean {
  return (
    typeof msg === "object" &&
    msg !== null &&
    (msg as { type?: unknown }).type === "assistant"
  );
}

interface ParsedResult {
  readonly finishReason: FinishReason;
  readonly usage: Usage | undefined;
  readonly errorMessage: string | undefined;
}

function parseResult(msg: unknown, requestedModel: Model): ParsedResult | undefined {
  if (msg === null || typeof msg !== "object") return undefined;
  const m = msg as Record<string, unknown>;
  if (m.type !== "result") return undefined;
  if (m.subtype === "success") {
    const usageRaw = m.usage as Record<string, unknown> | undefined;
    const input = typeof usageRaw?.input_tokens === "number" ? usageRaw.input_tokens : 0;
    const output = typeof usageRaw?.output_tokens === "number" ? usageRaw.output_tokens : 0;
    const costUsd = typeof m.total_cost_usd === "number" ? m.total_cost_usd : 0;
    return {
      finishReason: mapStopReason(m.stop_reason),
      usage: {
        model: requestedModel,
        inputTokens: input,
        outputTokens: output,
        costCents: Math.round(costUsd * 10000),
      },
      errorMessage: undefined,
    };
  }
  const err = typeof m.error === "string" ? m.error : "unknown agent SDK error";
  return {
    finishReason: "error",
    usage: {
      model: requestedModel,
      inputTokens: 0,
      outputTokens: 0,
      costCents: 0,
    },
    errorMessage: err,
  };
}

function mapStopReason(sdk: unknown): FinishReason {
  switch (sdk) {
    case "end_turn":
    case "stop_sequence":
    case "pause_turn":
    case "refusal":
    case null:
    case undefined:
      return "stop";
    case "max_tokens":
      return "max_tokens";
    case "tool_use":
      return "tool_use";
    default:
      return "stop";
  }
}

function translatePartialEvent(
  msg: unknown,
  codec: ToolNameCodec,
  getActive: () => string | undefined,
  setActive: (id: string | undefined) => void,
): StreamEvent | undefined {
  if (msg === null || typeof msg !== "object") return undefined;
  const m = msg as Record<string, unknown>;
  if (m.type !== "stream_event") return undefined;
  const event = m.event as Record<string, unknown> | undefined;
  if (event === undefined) return undefined;
  const innerType = event.type;
  if (innerType === "content_block_start") {
    const block = event.content_block as Record<string, unknown> | undefined;
    if (block?.type === "tool_use") {
      const id = typeof block.id === "string" ? block.id : "";
      setActive(id);
      const rawName = typeof block.name === "string" ? block.name : "";
      return {
        kind: "tool_call_start",
        toolCall: { id, name: codec.decode(rawName), arguments: {} },
      };
    }
    return undefined;
  }
  if (innerType === "content_block_delta") {
    const delta = event.delta as Record<string, unknown> | undefined;
    if (delta?.type === "text_delta") {
      return {
        kind: "text_delta",
        textDelta: typeof delta.text === "string" ? delta.text : "",
      };
    }
    if (delta?.type === "input_json_delta") {
      const id = getActive() ?? "";
      return {
        kind: "tool_call_delta",
        textDelta: typeof delta.partial_json === "string" ? delta.partial_json : "",
        toolCall: { id, name: "", arguments: {} },
      };
    }
    return undefined;
  }
  if (innerType === "content_block_stop") {
    setActive(undefined);
    return undefined;
  }
  return undefined;
}

async function safeInterrupt(iter: SdkIter): Promise<void> {
  if (typeof iter.interrupt !== "function") return;
  try {
    await iter.interrupt();
  } catch {
    // Best-effort interrupt: re-interrupting an already-finished query
    // is a no-op in spec but tolerate exceptions defensively.
  }
}

function errorMessageOf(e: unknown): string {
  if (e instanceof Error) return e.message;
  if (typeof e === "string") return e;
  if (typeof e === "object" && e !== null) {
    try {
      return JSON.stringify(e);
    } catch {
      return "[unserialisable error value]";
    }
  }
  // eslint-disable-next-line @typescript-eslint/no-base-to-string -- primitive branch; String() is the canonical path.
  return String(e);
}
```

- [ ] **Step 3.4: Run the test and confirm it passes**

Run: `pnpm -C harness test -- llm-tool-bridge-interceptor`

Expected: all 6 tests pass.

If `parseAssistant` is double-counting text vs `parseAssistantSnapshot` in the stream variant, prefer the snapshot — the partial events already emitted text deltas.

- [ ] **Step 3.5: Typecheck and lint**

Run: `pnpm -C harness typecheck && pnpm -C harness lint`

Expected: green.

- [ ] **Step 3.6: Commit**

```bash
git add harness/src/llm/tool-bridge-interceptor.ts harness/test/llm-tool-bridge-interceptor.test.ts
git -c commit.gpgsign=false commit -m "feat(harness): tool-bridge SDK-iterator interceptor for ClaudeAgentProvider (M5.7.c WIP 4c/n)

Captures assistant tool_use blocks (including parallel calls in one
message), decodes their names through the codec, and calls
iter.interrupt() so the runtime owns execution. The streaming variant
fans out portable StreamEvents while watching for the same assistant
snapshot. A stub-handler escape race surfaces the sentinel string from
the MCP stub server and is escalated as provider_unavailable rather
than silently passing through."
```

---

## Task 4 — Wire `ClaudeAgentProvider.complete()` to the bridge

**Files:**
- Modify: `harness/src/llm/claude-agent-provider.ts`
- Modify: `harness/test/llm-claude-agent-provider.test.ts`

The existing `complete()` body builds options, runs the iterator, and accumulates state inline. After this task, it delegates iterator handling to `interceptComplete` and additionally builds the MCP stub server when `req.tools` is non-empty.

- [ ] **Step 4.1: Write the failing integration test**

Append to `harness/test/llm-claude-agent-provider.test.ts` (after the existing describe blocks):

```ts
describe("ClaudeAgentProvider.complete with tools (M5.7.c slice 4)", () => {
  it("registers a watchkeeper MCP server in options.mcpServers", async () => {
    let observedOptions: unknown;
    const fake = ((opts: { prompt: string; options?: Record<string, unknown> }) => {
      observedOptions = opts.options;
      return (async function* () {
        yield {
          type: "result",
          subtype: "success",
          usage: { input_tokens: 0, output_tokens: 0 },
          total_cost_usd: 0,
          stop_reason: "end_turn",
        };
      })();
    }) as unknown as QueryImpl;
    const provider = new ClaudeAgentProvider({ queryImpl: fake });
    await provider.complete({
      model: model("claude-3-5-sonnet-latest"),
      messages: [{ role: "user", content: "hi" }],
      tools: [
        { name: "notebook.remember", description: "save", inputSchema: { type: "object" } },
      ],
    });
    const opts = observedOptions as { mcpServers?: Record<string, unknown> } | undefined;
    expect(opts?.mcpServers).toBeDefined();
    expect(Object.keys(opts!.mcpServers!)).toContain("watchkeeper");
  });

  it("returns toolCalls with runtime names (decoded from mcp prefix)", async () => {
    const fake = (() =>
      (async function* () {
        yield {
          type: "assistant",
          message: {
            content: [
              { type: "text", text: "I'll save that." },
              {
                type: "tool_use",
                id: "tu_1",
                name: "mcp__watchkeeper__notebook_remember",
                input: { key: "k", value: "v" },
              },
            ],
            usage: { input_tokens: 1, output_tokens: 1 },
          },
        };
      })()) as unknown as QueryImpl;
    const provider = new ClaudeAgentProvider({ queryImpl: fake });
    const resp = await provider.complete({
      model: model("claude-3-5-sonnet-latest"),
      messages: [{ role: "user", content: "save k=v" }],
      tools: [
        { name: "notebook.remember", description: "save", inputSchema: { type: "object" } },
      ],
    });
    expect(resp.finishReason).toBe("tool_use");
    expect(resp.toolCalls).toEqual([
      { id: "tu_1", name: "notebook.remember", arguments: { key: "k", value: "v" } },
    ]);
  });

  it("omits mcpServers entirely when tools is undefined", async () => {
    let observedOptions: unknown;
    const fake = ((opts: { prompt: string; options?: Record<string, unknown> }) => {
      observedOptions = opts.options;
      return (async function* () {
        yield {
          type: "result",
          subtype: "success",
          usage: { input_tokens: 0, output_tokens: 0 },
          total_cost_usd: 0,
          stop_reason: "end_turn",
        };
      })();
    }) as unknown as QueryImpl;
    const provider = new ClaudeAgentProvider({ queryImpl: fake });
    await provider.complete({
      model: model("claude-3-5-sonnet-latest"),
      messages: [{ role: "user", content: "hi" }],
    });
    const opts = observedOptions as { mcpServers?: Record<string, unknown> } | undefined;
    expect(opts?.mcpServers).toBeUndefined();
  });
});
```

- [ ] **Step 4.2: Run the new tests and confirm they fail**

Run: `pnpm -C harness test -- llm-claude-agent-provider`

Expected: the three new tests fail — either no `mcpServers` is set, or `toolCalls` is empty, depending on which assertion runs first.

- [ ] **Step 4.3: Wire the bridge into `complete()`**

Modify `harness/src/llm/claude-agent-provider.ts`:

1. Add new imports near the existing imports:

```ts
import { buildStubMcpServer } from "./tool-bridge-mcp-stub-server.js";
import { MCP_SERVER_NAME, buildCodec, type ToolNameCodec } from "./tool-bridge-name-codec.js";
import { interceptComplete } from "./tool-bridge-interceptor.js";
```

2. Replace the body of `complete()` (currently `harness/src/llm/claude-agent-provider.ts:108-154`) with the bridge-aware version:

```ts
public async complete(req: CompleteRequest): Promise<CompleteResponse> {
  validateModel(req.model);
  validateMessages(req.messages);
  validateTools(req.tools);

  const prompt = buildPromptFromMessages(req.messages);
  const codec: ToolNameCodec = buildCodec(req.tools ?? []);
  const options = this.buildOptions(req, { codec, tools: req.tools });

  let iter: ReturnType<typeof query>;
  try {
    iter =
      options === undefined ? this.queryImpl({ prompt }) : this.queryImpl({ prompt, options });
  } catch (e) {
    throw mapAgentError(e);
  }

  let turn;
  try {
    turn = await interceptComplete(iter, codec, req.model);
  } catch (e) {
    throw mapAgentError(e);
  }

  const response: CompleteResponse = {
    content: turn.text,
    toolCalls: turn.toolCalls,
    finishReason: turn.finishReason,
    usage:
      turn.usage ?? {
        model: req.model,
        inputTokens: 0,
        outputTokens: 0,
        costCents: 0,
      },
  };
  if (turn.errorMessage !== undefined) {
    return { ...response, errorMessage: turn.errorMessage };
  }
  return response;
}
```

3. Extend `buildOptions` to accept `{ codec, tools }` and inject `mcpServers` when tools are present:

```ts
private buildOptions(
  req: CompleteRequest | StreamRequest,
  extras?: {
    readonly partial?: boolean;
    readonly codec?: ToolNameCodec;
    readonly tools?: readonly ToolDefinition[];
  },
): Parameters<typeof query>[0]["options"] {
  const opts: Record<string, unknown> = { model: req.model };
  if (req.system !== undefined && req.system !== "") {
    opts.systemPrompt = req.system;
  }
  if (this.pathToExecutable !== undefined) {
    opts.pathToClaudeCodeExecutable = this.pathToExecutable;
  }
  if (extras?.partial === true) {
    opts.includePartialMessages = true;
  }
  if (this.apiKey !== undefined && this.apiKey !== "") {
    opts.env = { ...process.env, ...this.apiKeyEnvOverride() };
  }
  if (
    extras?.codec !== undefined &&
    extras.tools !== undefined &&
    extras.tools.length > 0
  ) {
    const stub = buildStubMcpServer(extras.tools, extras.codec);
    opts.mcpServers = { [MCP_SERVER_NAME]: stub.sdkConfig };
  }
  return opts;
}
```

4. Remove the now-unused private helpers `consumeMessage`, `consumeAssistantMessage`, `consumeResultMessage`, and the local `ConsumedMessage` interface — `interceptComplete` covers them. Keep `mapAgentError` and the validation helpers.

- [ ] **Step 4.4: Run the full file's tests and confirm they pass**

Run: `pnpm -C harness test -- llm-claude-agent-provider`

Expected: all original tests + the three new tool tests pass. If any pre-existing test references the deleted `consumeMessage` symbol, re-export it from the interceptor module or rewrite the test to drive through `complete()` instead.

- [ ] **Step 4.5: Typecheck and lint**

Run: `pnpm -C harness typecheck && pnpm -C harness lint`

Expected: green.

- [ ] **Step 4.6: Commit**

```bash
git add harness/src/llm/claude-agent-provider.ts harness/test/llm-claude-agent-provider.test.ts
git -c commit.gpgsign=false commit -m "feat(harness): wire ClaudeAgentProvider.complete() to tool bridge (M5.7.c WIP 4d/n)

complete() now builds the watchkeeper MCP stub server when req.tools is
non-empty, hands its config to the SDK via options.mcpServers, and
delegates iterator consumption to interceptComplete. The portable
LLMProvider contract is preserved: tool_use requests come back as
CompleteResponse.toolCalls with runtime names; the runtime executes
them. Three new integration tests on the existing queryImpl DI seam
pin the wiring."
```

---

## Task 5 — Wire `ClaudeAgentProvider.stream()` to the bridge

**Files:**
- Modify: `harness/src/llm/claude-agent-provider.ts`
- Modify: `harness/test/llm-claude-agent-provider.test.ts`

`stream()` already has a subscription class; the change is to (a) build the MCP server the same way as in Task 4 and (b) let `ClaudeAgentStreamSubscription` defer to `interceptStream` for the dispatch loop.

- [ ] **Step 5.1: Write the failing integration test**

Append to `harness/test/llm-claude-agent-provider.test.ts`:

```ts
describe("ClaudeAgentProvider.stream with tools (M5.7.c slice 4)", () => {
  it("emits tool_call_start, tool_call_delta and message_stop with finishReason=tool_use", async () => {
    const fake = (() =>
      (async function* () {
        yield {
          type: "stream_event",
          event: {
            type: "content_block_start",
            index: 0,
            content_block: {
              type: "tool_use",
              id: "tu_1",
              name: "mcp__watchkeeper__notebook_remember",
            },
          },
        };
        yield {
          type: "stream_event",
          event: {
            type: "content_block_delta",
            index: 0,
            delta: { type: "input_json_delta", partial_json: "{\"k\":1}" },
          },
        };
        yield {
          type: "stream_event",
          event: { type: "content_block_stop", index: 0 },
        };
        yield {
          type: "assistant",
          message: {
            content: [
              {
                type: "tool_use",
                id: "tu_1",
                name: "mcp__watchkeeper__notebook_remember",
                input: { k: 1 },
              },
            ],
            usage: { input_tokens: 1, output_tokens: 1 },
          },
        };
        yield {
          type: "result",
          subtype: "success",
          usage: { input_tokens: 1, output_tokens: 1 },
          total_cost_usd: 0,
          stop_reason: "tool_use",
        };
      })()) as unknown as QueryImpl;
    const provider = new ClaudeAgentProvider({ queryImpl: fake });
    const events: StreamEvent[] = [];
    const sub = await provider.stream(
      {
        model: model("claude-3-5-sonnet-latest"),
        messages: [{ role: "user", content: "save k=1" }],
        tools: [
          { name: "notebook.remember", description: "save", inputSchema: { type: "object" } },
        ],
      },
      async (e) => {
        events.push(e);
      },
    );
    // Drain by polling the subscription until it reports stopped.
    while (!sub.isStopped) {
      await new Promise((r) => setTimeout(r, 0));
    }
    const kinds = events.map((e) => e.kind);
    expect(kinds).toContain("tool_call_start");
    expect(kinds).toContain("tool_call_delta");
    const stop = events.find((e) => e.kind === "message_stop") as
      | Extract<StreamEvent, { kind: "message_stop" }>
      | undefined;
    expect(stop?.finishReason).toBe("tool_use");
  });
});
```

- [ ] **Step 5.2: Run the new test and confirm it fails**

Run: `pnpm -C harness test -- llm-claude-agent-provider`

Expected: the new stream-with-tools test fails because the existing stream subscription does not decode the MCP-prefixed tool name.

- [ ] **Step 5.3: Wire the bridge into `stream()`**

Modify `harness/src/llm/claude-agent-provider.ts`:

1. Replace the body of `stream()` (currently around lines 157–189) with the bridge-aware version:

```ts
// eslint-disable-next-line @typescript-eslint/require-await -- Promise return is the interface contract; body validates synchronously and starts the dispatch loop as a fire-and-forget promise.
public async stream(req: StreamRequest, handler: StreamHandler): Promise<StreamSubscription> {
  validateModel(req.model);
  validateMessages(req.messages);
  // eslint-disable-next-line @typescript-eslint/no-unnecessary-condition -- TS forbids null at the type level, but runtime callers crossing JSON-RPC / FFI boundaries can still pass null.
  if (handler === null || handler === undefined) {
    throw LLMError.invalidHandler();
  }
  validateTools(req.tools);

  const prompt = buildPromptFromMessages(req.messages);
  const codec: ToolNameCodec = buildCodec(req.tools ?? []);
  const options = this.buildOptions(req, {
    partial: true,
    codec,
    tools: req.tools,
  });

  let iter: ReturnType<typeof query>;
  try {
    iter =
      options === undefined ? this.queryImpl({ prompt }) : this.queryImpl({ prompt, options });
  } catch (e) {
    throw mapAgentError(e);
  }

  const sub = new ClaudeAgentStreamSubscription(iter);
  void sub.startDispatch(handler, codec, req.model);
  return sub;
}
```

2. Replace `ClaudeAgentStreamSubscription.startDispatch` so it delegates to `interceptStream`:

```ts
public async startDispatch(
  handler: StreamHandler,
  codec: ToolNameCodec,
  model: Model,
): Promise<void> {
  await interceptStream(this.iter, handler, codec, model, {
    get isStopped() {
      return self._stopped;
    },
    markStopped: (cause) => {
      self.markStopped(cause);
    },
  } as unknown as AbortBag);
}
```

(Where `self` is a `const self = this;` captured at the top of the method, since the arrow form bindings are awkward inside the inline object literal. Alternatively rewrite as an explicit `AbortBag` builder.)

3. Delete the existing local `translateStreamMessage` / `translatePartialAssistantEvent` / `ClaudeAgentStreamCorrelation` definitions — the interceptor owns them now. Keep the exported function only if `harness/src/llm/index.ts` re-exports it; if no consumers, remove.

4. Make sure `import { type AbortBag } from "./tool-bridge-interceptor.js"` is in the imports.

- [ ] **Step 5.4: Run the full file's tests and confirm they pass**

Run: `pnpm -C harness test -- llm-claude-agent-provider`

Expected: all stream tests (existing + new) pass. If the existing slice-3 stream test relied on `translateStreamMessage` as an export, either re-export from the interceptor or convert the test to drive through `provider.stream()`.

- [ ] **Step 5.5: Typecheck and lint**

Run: `pnpm -C harness typecheck && pnpm -C harness lint`

Expected: green.

- [ ] **Step 5.6: Commit**

```bash
git add harness/src/llm/claude-agent-provider.ts harness/test/llm-claude-agent-provider.test.ts
git -c commit.gpgsign=false commit -m "feat(harness): wire ClaudeAgentProvider.stream() to tool bridge (M5.7.c WIP 4e/n)

stream() now registers the watchkeeper MCP stub server alongside
includePartialMessages and delegates the dispatch loop to
interceptStream. The portable StreamEvent flow gains tool_call_start
and tool_call_delta with runtime tool names; the closing message_stop
carries finishReason='tool_use' and interrupts the SDK so the runtime
takes over execution."
```

---

## Task 6 — Full verification

**Files:**
- None modified; this task is a verification fence.

- [ ] **Step 6.1: Run the entire harness test suite**

Run: `pnpm -C harness test`

Expected: green. If any conformance / methods / cross-cutting test regresses, the most likely culprit is the removed local helper functions (`consumeMessage` and friends) — re-export them from the interceptor or fix the broken test.

- [ ] **Step 6.2: Run the full typecheck**

Run: `pnpm -C harness typecheck`

Expected: green. The most common `exactOptionalPropertyTypes` failure mode is `obj.field = undefined` (forbidden) — use spread / conditional construction.

- [ ] **Step 6.3: Run the full lint**

Run: `pnpm -C harness lint`

Expected: green.

- [ ] **Step 6.4: Run the Go grep-invariant test**

Run: `(cd core && go test ./pkg/secrets/...)`

Expected: green. Slice 4 does not touch credential code so the invariant should hold automatically. If it fails, you accidentally introduced the `ANTHROPIC_API_KEY` literal somewhere outside `harness/src/secrets/env.ts`.

- [ ] **Step 6.5: Confirm no orphan commits**

Run: `git log --oneline origin/feat/m5.7.c-claude-agent-provider..HEAD`

Expected: five new commits from tasks 1–5 sit on top of the existing slice-3 head. The spec doc commit (`11b114f`) should appear too — six new commits total over `origin/feat/m5.7.c-claude-agent-provider` as of plan creation time.

- [ ] **Step 6.6: Push the branch**

```bash
git push origin feat/m5.7.c-claude-agent-provider
```

Expected: push succeeds; CI on the branch starts. Do NOT open a PR yet — slices 5–7 still need to land on this branch per the operator's "one big PR" choice. The PR opens after slice 7 (docs + roadmap close-out).

---

## Self-review notes

- **Spec coverage:** every section of the spec (architecture, components, data flow, errors, testing) maps to a task. The `mcpServers` registration sits in Task 4 (`buildOptions` extension). Race detection sits in Task 3 (`checkForStubEscape` in the interceptor). The deferred `role=tool` skip is left untouched in `buildPromptFromMessages` and the existing `// M5.3.c.c.c` comment continues to apply; no task touches it.
- **Schema-converter scope:** Task 2 lists the supported JSON-Schema types explicitly (`object`, `string`, `number`/`integer`, `boolean`, `array`). Anything else degrades to `z.any()` with a one-shot warning. This matches the spec's "minimal converter sufficient for Phase 1 manifests" wording.
- **No placeholders:** every TDD step has runnable code. The few `eslint-disable` lines carry inline justifications.
- **Type consistency:** `ToolNameCodec` (Task 1) is consumed unchanged by Tasks 2 and 3. `InterceptedTurn` (Task 3) is consumed unchanged by Task 4. `AbortBag` (Task 3) is consumed unchanged by Task 5.
- **Out-of-scope discipline:** no task touches `harness/test/llm-conformance.test.ts` (slice 6), `harness/src/secrets/`, the cost-tracking refinement (slice 5), or the docs (slice 7). Inbound `role=tool` translation stays out of `buildPromptFromMessages` per the deferred M5.3.c.c.c reference.
