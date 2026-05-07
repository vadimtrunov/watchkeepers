/**
 * Tests for the harness → Go direction of the JSON-RPC channel
 * (M5.5.d.a.a). Exercises {@link parseResponse}, the {@link RpcClient}
 * pending-request lifecycle, id correlation, error rejection, the
 * unknown-id log-and-drop guard, and concurrent-request correlation.
 *
 * Pure TS — the Go-side host is exercised in `core/pkg/harnessrpc/`
 * (host_test.go + host_integration_test.go). These tests drive the
 * client with a synchronous in-memory line buffer that doubles as the
 * Go-side stand-in: tests inspect the emitted line, then synthesize
 * the response the Go host would emit.
 */

import { describe, expect, it, vi } from "vitest";

import { parseResponse, RpcClient, RpcRequestError } from "../src/jsonrpc.js";
import { JSON_RPC_VERSION, JsonRpcErrorCode } from "../src/types.js";

function decodeLine(line: string | undefined): { id: number } & Record<string, unknown> {
  if (line === undefined) {
    throw new Error("expected a written line");
  }
  return JSON.parse(line.slice(0, -1)) as { id: number } & Record<string, unknown>;
}

describe("parseResponse", () => {
  it("accepts a well-formed success response", () => {
    const result = parseResponse('{"jsonrpc":"2.0","id":1,"result":{"text":"hi"}}');
    expect(result.kind).toBe("ok");
    if (result.kind === "ok" && "result" in result.response) {
      expect(result.response.id).toBe(1);
      expect(result.response.result).toEqual({ text: "hi" });
    } else {
      throw new Error("expected ok success response");
    }
  });

  it("accepts a well-formed error response", () => {
    const result = parseResponse(
      '{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"method not found: foo"}}',
    );
    expect(result.kind).toBe("ok");
    if (result.kind === "ok" && "error" in result.response) {
      expect(result.response.id).toBe(2);
      expect(result.response.error.code).toBe(JsonRpcErrorCode.MethodNotFound);
      expect(result.response.error.message).toBe("method not found: foo");
    } else {
      throw new Error("expected ok error response");
    }
  });

  it("returns ParseError on malformed JSON", () => {
    const result = parseResponse("garbage");
    expect(result.kind).toBe("error");
    if (result.kind === "error") {
      expect(result.code).toBe(JsonRpcErrorCode.ParseError);
      expect(result.id).toBeNull();
    }
  });

  it("returns InvalidRequest when body is not an object", () => {
    const result = parseResponse("[1,2,3]");
    expect(result.kind).toBe("error");
    if (result.kind === "error") {
      expect(result.code).toBe(JsonRpcErrorCode.InvalidRequest);
    }
  });

  it("returns InvalidRequest when jsonrpc is wrong", () => {
    const result = parseResponse('{"jsonrpc":"1.0","id":1,"result":{}}');
    expect(result.kind).toBe("error");
  });

  it("returns InvalidRequest when both result and error are present", () => {
    const result = parseResponse(
      '{"jsonrpc":"2.0","id":1,"result":{},"error":{"code":-1,"message":"bad"}}',
    );
    expect(result.kind).toBe("error");
    if (result.kind === "error") {
      expect(result.code).toBe(JsonRpcErrorCode.InvalidRequest);
    }
  });

  it("returns InvalidRequest when neither result nor error is present", () => {
    const result = parseResponse('{"jsonrpc":"2.0","id":1}');
    expect(result.kind).toBe("error");
  });

  it("returns InvalidRequest when error.code is missing", () => {
    const result = parseResponse('{"jsonrpc":"2.0","id":1,"error":{"message":"x"}}');
    expect(result.kind).toBe("error");
  });

  it("preserves error.data when present", () => {
    const result = parseResponse(
      '{"jsonrpc":"2.0","id":3,"error":{"code":-32603,"message":"x","data":{"trace":"abc"}}}',
    );
    expect(result.kind).toBe("ok");
    if (result.kind === "ok" && "error" in result.response) {
      expect(result.response.error.data).toEqual({ trace: "abc" });
    }
  });
});

describe("RpcClient.request", () => {
  it("writes a correctly framed request line", () => {
    const written: string[] = [];
    const client = new RpcClient((line) => written.push(line));

    void client.request("echo", { text: "hi" });

    expect(written).toHaveLength(1);
    const line = written[0];
    if (line === undefined) {
      throw new Error("expected one written line");
    }
    expect(line.endsWith("\n")).toBe(true);
    const parsed = JSON.parse(line.slice(0, -1)) as {
      jsonrpc: string;
      id: number;
      method: string;
      params: { text: string };
    };
    expect(parsed.jsonrpc).toBe(JSON_RPC_VERSION);
    expect(parsed.method).toBe("echo");
    expect(parsed.params).toEqual({ text: "hi" });
    expect(parsed.id).toBe(1);
  });

  it("auto-increments numeric ids per request", () => {
    const written: string[] = [];
    const client = new RpcClient((line) => written.push(line));

    void client.request("a");
    void client.request("b");
    void client.request("c");

    const ids = written.map((line) => decodeLine(line).id);
    expect(ids).toEqual([1, 2, 3]);
  });

  it("omits params when not supplied", () => {
    const written: string[] = [];
    const client = new RpcClient((line) => written.push(line));

    void client.request("ping");

    const parsed = decodeLine(written[0]);
    expect("params" in parsed).toBe(false);
  });

  it("resolves the pending promise on a matching success response", async () => {
    const written: string[] = [];
    const client = new RpcClient((line) => written.push(line));

    const pending = client.request("echo", { text: "hi" });
    const id = decodeLine(written[0]).id;

    client.handleResponseLine(`{"jsonrpc":"2.0","id":${String(id)},"result":{"text":"hi"}}\n`);

    await expect(pending).resolves.toEqual({ text: "hi" });
    expect(client.pendingCount()).toBe(0);
  });

  it("rejects with RpcRequestError on a matching error response", async () => {
    const written: string[] = [];
    const client = new RpcClient((line) => written.push(line));

    const pending = client.request("missing");
    const id = decodeLine(written[0]).id;

    client.handleResponseLine(
      `{"jsonrpc":"2.0","id":${String(id)},"error":{"code":-32601,"message":"method not found: missing"}}\n`,
    );

    await expect(pending).rejects.toBeInstanceOf(RpcRequestError);
    await expect(pending).rejects.toMatchObject({
      code: JsonRpcErrorCode.MethodNotFound,
      message: "method not found: missing",
    });
    expect(client.pendingCount()).toBe(0);
  });

  it("logs-and-drops a response with an unknown id (no throw)", () => {
    const written: string[] = [];
    const onUnknown = vi.fn();
    const client = new RpcClient((line) => written.push(line), onUnknown);

    const result = client.handleResponseLine('{"jsonrpc":"2.0","id":42,"result":{}}\n');

    expect(result.kind).toBe("ok");
    expect(onUnknown).toHaveBeenCalledTimes(1);
    const firstCall = onUnknown.mock.calls[0];
    if (firstCall === undefined) {
      throw new Error("expected onUnknown call");
    }
    expect(firstCall[0]).toBe(42);
    expect(client.pendingCount()).toBe(0);
  });

  it("logs-and-drops a response with a null id (parse-error envelope)", () => {
    const onUnknown = vi.fn();
    const client = new RpcClient(() => undefined, onUnknown);

    client.handleResponseLine(
      '{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}\n',
    );

    expect(onUnknown).toHaveBeenCalledTimes(1);
    const firstCall = onUnknown.mock.calls[0];
    if (firstCall === undefined) {
      throw new Error("expected onUnknown call");
    }
    expect(firstCall[0]).toBeNull();
  });

  it("does not feed malformed lines to the pending map", async () => {
    const written: string[] = [];
    const client = new RpcClient((line) => written.push(line));

    const pending = client.request("echo");
    const result = client.handleResponseLine("not json\n");

    expect(result.kind).toBe("error");
    expect(client.pendingCount()).toBe(1);

    // Resolve the pending request so the test does not leak a dangling
    // promise.
    const id = decodeLine(written[0]).id;
    client.handleResponseLine(`{"jsonrpc":"2.0","id":${String(id)},"result":null}\n`);
    await expect(pending).resolves.toBeNull();
  });

  it("correlates concurrent requests via Promise.all", async () => {
    const written: string[] = [];
    const client = new RpcClient((line) => written.push(line));

    const both = Promise.all([
      client.request("echo", { text: "a" }),
      client.request("echo", { text: "b" }),
    ]);

    expect(written).toHaveLength(2);
    const idA = decodeLine(written[0]).id;
    const idB = decodeLine(written[1]).id;
    expect(idA).not.toBe(idB);

    // Respond out of order to prove correlation is by id, not arrival.
    client.handleResponseLine(`{"jsonrpc":"2.0","id":${String(idB)},"result":{"text":"b"}}\n`);
    client.handleResponseLine(`{"jsonrpc":"2.0","id":${String(idA)},"result":{"text":"a"}}\n`);

    const [a, b] = await both;
    expect(a).toEqual({ text: "a" });
    expect(b).toEqual({ text: "b" });
    expect(client.pendingCount()).toBe(0);
  });

  it("removes the pending entry synchronously when the writer throws", async () => {
    const client = new RpcClient(() => {
      throw new Error("write failed");
    });

    const pending = client.request("echo");
    await expect(pending).rejects.toThrow(/write failed/);
    expect(client.pendingCount()).toBe(0);
  });
});
