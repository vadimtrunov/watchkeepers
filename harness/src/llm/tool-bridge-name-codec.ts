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
        throw LLMError.providerUnavailable(`agent SDK emitted unknown tool name: ${mcpName}`);
      }
      return runtime;
    },
  };
}
