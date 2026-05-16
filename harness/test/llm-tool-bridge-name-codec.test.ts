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
import { MCP_SERVER_NAME, buildCodec } from "../src/llm/tool-bridge-name-codec.js";
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
    expect(codec.encode("keepers.notebook.archive")).toBe("keepers_notebook_archive");
  });

  it("decodes the bare encoded form back to runtime name", () => {
    const codec = buildCodec([td("notebook.remember")]);
    expect(codec.decode("notebook_remember")).toBe("notebook.remember");
  });

  it("decodes the SDK-prefixed form back to runtime name", () => {
    const codec = buildCodec([td("notebook.remember")]);
    expect(codec.decode(`mcp__${MCP_SERVER_NAME}__notebook_remember`)).toBe("notebook.remember");
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
    expect(() => codec.decode("mcp__other__notebook_remember")).toThrow(LLMError);
  });
});
