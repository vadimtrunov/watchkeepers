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
 * manifests use today (`object` with `properties` / `required`), primitive
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

export interface StubToolEntry {
  readonly name: string;
  readonly handler: (args: Record<string, unknown>, extra: unknown) => Promise<never>;
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
    const shape = jsonSchemaToZodShape(t.inputSchema ?? undefined);
    const handler: StubToolEntry["handler"] = () =>
      Promise.reject(
        new Error(
          `${MCP_STUB_SENTINEL}: ${t.name} (encoded ${encoded}); the interceptor was supposed to capture this tool_use before dispatch.`,
        ),
      );
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
function jsonSchemaToZodShape(schema: Record<string, unknown> | undefined): ZodRawShape {
  if (schema?.type !== "object") return {};
  const props = (schema.properties as Record<string, Record<string, unknown>> | undefined) ?? {};
  const required = new Set((schema.required as string[] | undefined) ?? []);
  const shape: ZodRawShape = {};
  for (const [key, propSchema] of Object.entries(props)) {
    const base = jsonSchemaToZod(propSchema);
    shape[key] = required.has(key) ? base : base.optional();
  }
  return shape;
}

const warnedSchemas = new WeakSet<object>();

/**
 * Emit a one-shot warn for an unsupported JSON-Schema shape.
 *
 * Dedup is keyed on object identity, not on schema content. In the
 * production path `buildStubMcpServer` passes the same `t.inputSchema`
 * reference per tool per request, so this fires at most once per build
 * — which is the contract the spec promises. Ad-hoc callers of
 * `jsonSchemaToZod` with freshly-built object literals will warn each
 * call; that's intentionally permissive because the warn is a
 * diagnostic, not a control-flow signal.
 */
function warnUnsupportedSchema(schema: Record<string, unknown>): void {
  if (warnedSchemas.has(schema)) return;
  warnedSchemas.add(schema);
  console.warn(
    `tool-bridge: unsupported JSON-Schema shape, falling back to z.any(): ${JSON.stringify(
      schema,
    )}`,
  );
}
