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

import {
  MCP_STUB_SENTINEL,
  buildStubMcpServer,
  jsonSchemaToZod,
} from "../src/llm/tool-bridge-mcp-stub-server.js";
import { MCP_SERVER_NAME, buildCodec } from "../src/llm/tool-bridge-name-codec.js";
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
    const firstTool = cfg.tools[0];
    if (firstTool === undefined) throw new Error("no tools registered");
    const handler = firstTool.handler;
    await expect(handler({}, undefined)).rejects.toThrowError(new RegExp(MCP_STUB_SENTINEL));
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
