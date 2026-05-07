/**
 * Unit tests for the {@link looksLikeResponse} classifier used in the
 * `runHarness` readline loop (M5.5.d.b). The classifier gates which
 * inbound NDJSON lines are routed to {@link RpcClient#handleResponseLine}
 * vs. the standard request dispatcher.
 *
 * Table-driven cases exercise the four JSON-RPC message shapes plus the
 * two defensive fall-through cases (malformed JSON, ambiguous mix).
 */

import { describe, expect, it } from "vitest";

import { looksLikeResponse } from "../src/index.js";

describe("looksLikeResponse", () => {
  it("returns false for a request (has method + id)", () => {
    const line = JSON.stringify({ jsonrpc: "2.0", id: 1, method: "invokeTool", params: {} });
    expect(looksLikeResponse(line)).toBe(false);
  });

  it("returns false for a notification (has method, no id) — method-precedence check", () => {
    const line = JSON.stringify({ jsonrpc: "2.0", method: "harness/ready", params: {} });
    expect(looksLikeResponse(line)).toBe(false);
  });

  it("returns true for a success response (has result, no method)", () => {
    const line = JSON.stringify({ jsonrpc: "2.0", id: 1, result: { accepted: true } });
    expect(looksLikeResponse(line)).toBe(true);
  });

  it("returns true for an error response (has error, no method)", () => {
    const line = JSON.stringify({
      jsonrpc: "2.0",
      id: 1,
      error: { code: -32601, message: "Method not found" },
    });
    expect(looksLikeResponse(line)).toBe(true);
  });

  it("returns false for malformed JSON — falls through to dispatcher (ParseError path)", () => {
    expect(looksLikeResponse("not json at all {{{")).toBe(false);
  });

  it("returns false for ambiguous object with both method and result — method-precedence wins", () => {
    const line = JSON.stringify({ jsonrpc: "2.0", id: 2, method: "foo", result: "bar" });
    expect(looksLikeResponse(line)).toBe(false);
  });
});
