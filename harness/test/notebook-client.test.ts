/**
 * Tests for the {@link rememberEntry} client wrapper (M5.5.d.a.b).
 *
 * Drives {@link RpcClient} with an in-memory line buffer — no subprocess,
 * no Go host. Tests inspect the emitted NDJSON line and synthesize the
 * response the Go host would emit so the promise settles synchronously.
 */

import { describe, expect, it } from "vitest";

import { RpcClient, RpcRequestError } from "../src/jsonrpc.js";
import { rememberEntry } from "../src/notebookClient.js";
import { JSON_RPC_VERSION } from "../src/types.js";

interface RequestEnvelope {
  id: number;
  method: string;
  params: Record<string, string>;
}

/** Decode the single line emitted by RpcClient.request (strips trailing LF). */
function decodeLine(line: string | undefined): RequestEnvelope {
  if (line === undefined) throw new Error("expected a written line");
  return JSON.parse(line.endsWith("\n") ? line.slice(0, -1) : line) as RequestEnvelope;
}

/**
 * Build an in-memory RpcClient with a captured-lines array and a helper
 * that synthesises a response line (simulating the Go host reply).
 */
function makeClient(): {
  rpc: RpcClient;
  lines: string[];
  respond: (id: number, result: unknown) => void;
  respondError: (id: number, code: number, message: string) => void;
} {
  const lines: string[] = [];
  const rpc = new RpcClient((line) => lines.push(line));

  const respond = (id: number, result: unknown): void => {
    const resp = JSON.stringify({ jsonrpc: JSON_RPC_VERSION, id, result }) + "\n";
    rpc.handleResponseLine(resp);
  };

  const respondError = (id: number, code: number, message: string): void => {
    const resp = JSON.stringify({ jsonrpc: JSON_RPC_VERSION, id, error: { code, message } }) + "\n";
    rpc.handleResponseLine(resp);
  };

  return { rpc, lines, respond, respondError };
}

describe("notebookClient_RememberEntry_SendsCorrectMethodAndParams", () => {
  it("emits notebook.remember with the supplied params", async () => {
    const { rpc, lines, respond } = makeClient();

    const promise = rememberEntry(rpc, {
      agentID: "agent-1",
      category: "lesson",
      subject: "Go testing",
      content: "Use t.TempDir",
    });

    // Synthesise Go-host response before awaiting so the promise settles.
    const decoded = decodeLine(lines[0]);
    respond(decoded.id, { id: "entry-uuid-1" });

    await promise;

    expect(decoded.method).toBe("notebook.remember");
    expect(decoded.params.agentID).toBe("agent-1");
    expect(decoded.params.category).toBe("lesson");
    expect(decoded.params.subject).toBe("Go testing");
    expect(decoded.params.content).toBe("Use t.TempDir");
  });
});

describe("notebookClient_RememberEntry_ResolvesWithId", () => {
  it("resolves with the {id} returned by the host", async () => {
    const { rpc, lines, respond } = makeClient();

    const promise = rememberEntry(rpc, {
      agentID: "agent-2",
      category: "observation",
      subject: "",
      content: "Some observation",
    });

    const decoded = decodeLine(lines[0]);
    respond(decoded.id, { id: "the-returned-uuid" });

    const result = await promise;
    expect(result.id).toBe("the-returned-uuid");
  });
});

describe("notebookClient_RememberEntry_RejectsOnRpcError", () => {
  it("rejects with RpcRequestError when the host returns an error envelope", async () => {
    const { rpc, lines, respondError } = makeClient();

    const promise = rememberEntry(rpc, {
      agentID: "agent-3",
      category: "lesson",
      subject: "",
      content: "content",
    });

    const decoded = decodeLine(lines[0]);
    respondError(decoded.id, -32603, "internal error: agent not registered: agent-3");

    await expect(promise).rejects.toThrow(RpcRequestError);
    await expect(promise).rejects.toMatchObject({ code: -32603 });
  });
});
