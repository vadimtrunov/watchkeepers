/**
 * Wire-format tests — pure parser and serializer, no I/O.
 *
 * Coverage targets the JSON-RPC 2.0 envelope: parse error → -32700,
 * malformed request → -32600, well-formed request round trip, response
 * and notification serialization end with LF.
 */

import { describe, expect, it } from "vitest";

import {
  errorResponse,
  notification,
  parseRequest,
  serialize,
  successResponse,
} from "../src/jsonrpc.js";
import { JSON_RPC_VERSION, JsonRpcErrorCode } from "../src/types.js";

describe("parseRequest", () => {
  it("accepts a well-formed request", () => {
    const result = parseRequest('{"jsonrpc":"2.0","id":1,"method":"hello"}');
    expect(result.kind).toBe("ok");
    if (result.kind === "ok") {
      expect(result.request.id).toBe(1);
      expect(result.request.method).toBe("hello");
      expect(result.request.params).toBeUndefined();
    }
  });

  it("preserves params when present", () => {
    const result = parseRequest('{"jsonrpc":"2.0","id":"abc","method":"echo","params":{"x":1}}');
    expect(result.kind).toBe("ok");
    if (result.kind === "ok") {
      expect(result.request.params).toEqual({ x: 1 });
    }
  });

  it("returns ParseError on malformed JSON", () => {
    const result = parseRequest("not json");
    expect(result.kind).toBe("error");
    if (result.kind === "error") {
      expect(result.code).toBe(JsonRpcErrorCode.ParseError);
      expect(result.id).toBeNull();
    }
  });

  it("returns InvalidRequest when body is not an object", () => {
    const result = parseRequest("[1,2,3]");
    expect(result.kind).toBe("error");
    if (result.kind === "error") {
      expect(result.code).toBe(JsonRpcErrorCode.InvalidRequest);
      expect(result.id).toBeNull();
    }
  });

  it("returns InvalidRequest when jsonrpc is wrong", () => {
    const result = parseRequest('{"jsonrpc":"1.0","id":1,"method":"hello"}');
    expect(result.kind).toBe("error");
    if (result.kind === "error") {
      expect(result.code).toBe(JsonRpcErrorCode.InvalidRequest);
      // id is recoverable here
      expect(result.id).toBe(1);
    }
  });

  it("returns InvalidRequest when method is missing", () => {
    const result = parseRequest('{"jsonrpc":"2.0","id":1}');
    expect(result.kind).toBe("error");
    if (result.kind === "error") {
      expect(result.code).toBe(JsonRpcErrorCode.InvalidRequest);
    }
  });

  it("returns InvalidRequest when method is empty", () => {
    const result = parseRequest('{"jsonrpc":"2.0","id":1,"method":""}');
    expect(result.kind).toBe("error");
    if (result.kind === "error") {
      expect(result.code).toBe(JsonRpcErrorCode.InvalidRequest);
    }
  });

  it("returns InvalidRequest when id is an object", () => {
    const result = parseRequest('{"jsonrpc":"2.0","id":{},"method":"hello"}');
    expect(result.kind).toBe("error");
    if (result.kind === "error") {
      expect(result.code).toBe(JsonRpcErrorCode.InvalidRequest);
    }
  });

  it("parses an id-less envelope as a notification (JSON-RPC §4.1)", () => {
    const result = parseRequest('{"jsonrpc":"2.0","method":"event.tick"}');
    expect(result.kind).toBe("notification");
    if (result.kind === "notification") {
      expect(result.notification.method).toBe("event.tick");
      expect(result.notification.params).toBeUndefined();
    }
  });

  it("preserves params on a parsed notification", () => {
    const result = parseRequest('{"jsonrpc":"2.0","method":"event.tick","params":{"n":1}}');
    expect(result.kind).toBe("notification");
    if (result.kind === "notification") {
      expect(result.notification.params).toEqual({ n: 1 });
    }
  });

  it("accepts null id", () => {
    const result = parseRequest('{"jsonrpc":"2.0","id":null,"method":"hello"}');
    expect(result.kind).toBe("ok");
    if (result.kind === "ok") {
      expect(result.request.id).toBeNull();
    }
  });

  it("accepts string id", () => {
    const result = parseRequest('{"jsonrpc":"2.0","id":"req-1","method":"hello"}');
    expect(result.kind).toBe("ok");
    if (result.kind === "ok") {
      expect(result.request.id).toBe("req-1");
    }
  });
});

describe("serialize", () => {
  it("appends LF to a success response", () => {
    const line = serialize(successResponse(1, { ok: true }));
    expect(line.endsWith("\n")).toBe(true);
    expect(JSON.parse(line.slice(0, -1))).toEqual({
      jsonrpc: JSON_RPC_VERSION,
      id: 1,
      result: { ok: true },
    });
  });

  it("appends LF to an error response", () => {
    const line = serialize(errorResponse(1, JsonRpcErrorCode.MethodNotFound, "no such method"));
    expect(line.endsWith("\n")).toBe(true);
    const decoded = JSON.parse(line.slice(0, -1)) as {
      error: { code: number; message: string };
    };
    expect(decoded.error.code).toBe(JsonRpcErrorCode.MethodNotFound);
    expect(decoded.error.message).toBe("no such method");
  });

  it("appends LF to a notification", () => {
    const line = serialize(notification("event.tick", { count: 1 }));
    expect(line.endsWith("\n")).toBe(true);
    const decoded = JSON.parse(line.slice(0, -1)) as {
      jsonrpc: string;
      method: string;
      id?: unknown;
    };
    expect(decoded.method).toBe("event.tick");
    expect(decoded.jsonrpc).toBe(JSON_RPC_VERSION);
    expect("id" in decoded).toBe(false);
  });

  it("omits optional data field when not provided", () => {
    const line = serialize(errorResponse(1, JsonRpcErrorCode.ParseError, "bad json"));
    const decoded = JSON.parse(line.slice(0, -1)) as { error: Record<string, unknown> };
    expect("data" in decoded.error).toBe(false);
  });

  it("includes data field when provided", () => {
    const line = serialize(errorResponse(1, JsonRpcErrorCode.InvalidParams, "bad", { field: "x" }));
    const decoded = JSON.parse(line.slice(0, -1)) as {
      error: { data: { field: string } };
    };
    expect(decoded.error.data).toEqual({ field: "x" });
  });
});
