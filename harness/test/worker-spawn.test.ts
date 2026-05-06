/**
 * Worker spawn + IPC JSON-RPC transport tests (M5.3.b.b.c).
 *
 * Real `fork()` happy paths use the built `dist/worker/bootstrap.js`;
 * the test bootstrap accepts a `crashOnInit` hook so the negative
 * crash test reuses the same fixture. Transport-only paths use stub
 * IPC channels for determinism.
 */

import { execSync } from "node:child_process";
import { EventEmitter } from "node:events";
import { existsSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { afterEach, beforeAll, describe, expect, it } from "vitest";

import type { CapabilityDeclaration } from "../src/capabilities.js";
import { ToolErrorCode } from "../src/invokeTool.js";
import { runBootstrap } from "../src/worker/bootstrap.js";
import {
  spawnWorker,
  buildCrashEvent,
  type WorkerCrashEvent,
  type WorkerHandle,
} from "../src/worker/spawn.js";
import {
  IpcJsonRpcTransport,
  JsonRpcRemoteError,
  ipcChannelFromProcess,
  type IpcChannel,
} from "../src/worker/transport.js";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const HARNESS_ROOT = resolve(__dirname, "..");
const BOOTSTRAP_PATH = resolve(HARNESS_ROOT, "dist/worker/bootstrap.js");

const EMPTY_CAPS: CapabilityDeclaration = {
  fs: { read: [], write: [] },
  net: { allow: [] },
  env: { allow: [] },
  proc: { spawn: false },
};

beforeAll(() => {
  if (!existsSync(BOOTSTRAP_PATH)) {
    execSync("pnpm build", { cwd: HARNESS_ROOT, stdio: "inherit" });
  }
});

describe("spawnWorker — happy paths", () => {
  let worker: WorkerHandle | undefined;

  afterEach(async () => {
    if (worker !== undefined) {
      await worker.terminate();
      worker = undefined;
    }
  });

  it("resolves with a handle, ping returns 'pong', terminate is clean", async () => {
    worker = await spawnWorker(EMPTY_CAPS, { bootstrapPath: BOOTSTRAP_PATH });
    expect(await worker.request("ping")).toBe("pong");
    await worker.terminate();
    worker = undefined;
  });

  it("notify('log') returns sync, no spurious response", async () => {
    worker = await spawnWorker(EMPTY_CAPS, { bootstrapPath: BOOTSTRAP_PATH });
    expect(() => worker?.notify("log", { msg: "x" })).not.toThrow();
    // If the worker had replied to the notification, this ping's
    // correlation id would resolve to the spurious payload instead.
    expect(await worker.request("ping")).toBe("pong");
  });

  it("terminate() on an idle worker resolves within 1s", async () => {
    worker = await spawnWorker(EMPTY_CAPS, { bootstrapPath: BOOTSTRAP_PATH });
    const start = Date.now();
    await worker.terminate();
    worker = undefined;
    expect(Date.now() - start).toBeLessThan(1000);
  });

  it("accepts empty allowlists across every axis and stays responsive", async () => {
    worker = await spawnWorker(EMPTY_CAPS, { bootstrapPath: BOOTSTRAP_PATH });
    expect(await worker.request("ping")).toBe("pong");
  });
});

describe("spawnWorker — crash semantics", () => {
  it("emits one-shot 'crash' with code -32004 and exitCode=1 on unexpected exit", async () => {
    const worker = await spawnWorker(EMPTY_CAPS, {
      bootstrapPath: BOOTSTRAP_PATH,
      crashOnInit: true,
    });
    const events: WorkerCrashEvent[] = [];
    const handler = (e: WorkerCrashEvent): void => void events.push(e);
    worker.on("crash", handler);
    await new Promise<void>((res) => setTimeout(res, 200));
    expect(events).toHaveLength(1);
    expect(events[0]?.code).toBe(ToolErrorCode.ToolWorkerCrashed);
    expect(events[0]?.exitCode).toBe(1);
    worker.off("crash", handler);
  });
});

describe("IpcJsonRpcTransport — round trips", () => {
  it("request/response round-trips over a stub channel", async () => {
    const { parent, child } = makeChannelPair();
    const p = new IpcJsonRpcTransport(parent);
    const c = new IpcJsonRpcTransport(child);
    c.onRequest((req) => {
      c.sendResponse(req.id, "pong");
    });
    expect(await p.request("ping")).toBe("pong");
    p.dispose();
    c.dispose();
  });

  it("notification fires no response slot", async () => {
    const { parent, child } = makeChannelPair();
    const p = new IpcJsonRpcTransport(parent);
    const c = new IpcJsonRpcTransport(child);
    let observed = false;
    c.onNotification(() => (observed = true));
    p.notify("log", { msg: "hello" });
    await new Promise<void>((res) => setTimeout(res, 5));
    expect(observed).toBe(true);
    p.dispose();
    c.dispose();
  });

  it("rejects pending requests on dispose", async () => {
    const { parent, child } = makeChannelPair();
    const p = new IpcJsonRpcTransport(parent);
    const c = new IpcJsonRpcTransport(child);
    const pending = p.request("ping");
    p.dispose();
    await expect(pending).rejects.toThrow(/transport disposed/);
    c.dispose();
  });

  it("propagates JSON-RPC error envelopes as JsonRpcRemoteError", async () => {
    const { parent, child } = makeChannelPair();
    const p = new IpcJsonRpcTransport(parent);
    const c = new IpcJsonRpcTransport(child);
    c.onRequest((req) => {
      c.sendError(req.id, -32601, "method not found: nope");
    });
    await expect(p.request("nope")).rejects.toMatchObject({
      name: "JsonRpcRemoteError",
      code: -32601,
    });
    p.dispose();
    c.dispose();
  });
});

describe("IpcJsonRpcTransport — malformed inbound", () => {
  it("surfaces parse errors without throwing", () => {
    const { parent, child } = makeChannelPair();
    const p = new IpcJsonRpcTransport(parent);
    const errors: { reason: string }[] = [];
    p.onParseError((err) => errors.push({ reason: err.reason }));
    expect(() => {
      child.send("not an object");
      child.send({ jsonrpc: "1.0", id: 1, result: null });
      child.send({ jsonrpc: "2.0" });
      child.send(null);
      child.send([1, 2, 3]);
      child.send({ jsonrpc: "2.0", method: "" });
    }).not.toThrow();
    expect(errors.length).toBeGreaterThanOrEqual(2);
    p.dispose();
  });

  it("rejects a pending request when the response arrives with a malformed jsonrpc field", async () => {
    // Comment 1 regression: a bad jsonrpc version on a response-shaped envelope
    // must reject the awaiter rather than leaving it hanging.
    const { parent, child } = makeChannelPair();
    const p = new IpcJsonRpcTransport(parent);
    const c = new IpcJsonRpcTransport(child);
    // Intercept the outgoing request to learn its id, then reply with a bad jsonrpc version.
    c.onRequest((req) => {
      // Send a response with jsonrpc "1.0" instead of "2.0".
      child.send({ jsonrpc: "1.0", id: req.id, result: "bad" });
    });
    await expect(p.request("ping")).rejects.toThrow(/malformed response/);
    p.dispose();
    c.dispose();
  });

  it("JsonRpcRemoteError carries class identity and code/data", () => {
    const err = new JsonRpcRemoteError(-32000, "boom", { detail: 1 });
    expect(err).toBeInstanceOf(Error);
    expect(err).toBeInstanceOf(JsonRpcRemoteError);
    expect(err.code).toBe(-32000);
    expect(err.data).toEqual({ detail: 1 });
  });
});

describe("IpcJsonRpcTransport — send-failure resilience", () => {
  it("notify does not throw when channel.send throws synchronously (Comment 4)", () => {
    // Simulate ERR_IPC_CHANNEL_CLOSED on a closed channel.
    const tx = new IpcJsonRpcTransport({
      send: () => {
        throw new Error("ERR_IPC_CHANNEL_CLOSED");
      },
      onMessage: () => undefined,
      offMessage: () => undefined,
    });
    expect(() => { tx.notify("log", { msg: "x" }); }).not.toThrow();
    tx.dispose();
  });

  it("sendResponse does not throw when channel.send throws synchronously (Comment 4)", () => {
    const tx = new IpcJsonRpcTransport({
      send: () => {
        throw new Error("ERR_IPC_CHANNEL_CLOSED");
      },
      onMessage: () => undefined,
      offMessage: () => undefined,
    });
    expect(() => { tx.sendResponse(1, "ok"); }).not.toThrow();
    tx.dispose();
  });

  it("sendError does not throw when channel.send throws synchronously (Comment 4)", () => {
    const tx = new IpcJsonRpcTransport({
      send: () => {
        throw new Error("ERR_IPC_CHANNEL_CLOSED");
      },
      onMessage: () => undefined,
      offMessage: () => undefined,
    });
    expect(() => { tx.sendError(1, -32000, "boom"); }).not.toThrow();
    tx.dispose();
  });
});

describe("IpcJsonRpcTransport — additional coverage", () => {
  it("post-dispose: request rejects, notify/sendResponse/sendError are no-ops, dispose idempotent", async () => {
    const { parent } = makeChannelPair();
    const tx = new IpcJsonRpcTransport(parent);
    tx.dispose();
    await expect(tx.request("nope")).rejects.toThrow(/transport disposed/);
    expect(() => {
      tx.notify("a");
      tx.sendResponse(1, "r");
      tx.sendError(1, -32000, "x");
      tx.dispose();
    }).not.toThrow();
  });

  it("rejects request when channel.send throws synchronously", async () => {
    const tx = new IpcJsonRpcTransport({
      send: () => {
        throw new Error("ipc closed");
      },
      onMessage: () => undefined,
      offMessage: () => undefined,
    });
    await expect(tx.request("ping")).rejects.toThrow(/ipc closed/);
    tx.dispose();
  });

  it("forwards request and notification params verbatim", async () => {
    const { parent, child } = makeChannelPair();
    const p = new IpcJsonRpcTransport(parent);
    const c = new IpcJsonRpcTransport(child);
    let lastReq: unknown;
    let lastNotif: unknown;
    c.onRequest((req) => {
      lastReq = req.params;
      c.sendResponse(req.id, "ok");
    });
    c.onNotification((notif) => (lastNotif = notif.params));
    await p.request("with-params", { a: 1 });
    p.notify("note", { b: 2 });
    await new Promise<void>((res) => setTimeout(res, 5));
    expect(lastReq).toEqual({ a: 1 });
    expect(lastNotif).toEqual({ b: 2 });
    p.dispose();
    c.dispose();
  });

  it("sendError propagates `data` through to JsonRpcRemoteError.data", async () => {
    const { parent, child } = makeChannelPair();
    const p = new IpcJsonRpcTransport(parent);
    const c = new IpcJsonRpcTransport(child);
    c.onRequest((req) => {
      c.sendError(req.id, -32000, "with data", { extra: "info" });
    });
    const err = await p.request("nope").catch((e: unknown) => e);
    expect((err as JsonRpcRemoteError).data).toEqual({ extra: "info" });
    p.dispose();
    c.dispose();
  });

  it("surfaces parse errors for non-numeric / unknown / non-scalar response ids", () => {
    const { parent, child } = makeChannelPair();
    const p = new IpcJsonRpcTransport(parent);
    const errors: { reason: string }[] = [];
    p.onParseError((err) => errors.push({ reason: err.reason }));
    child.send({ jsonrpc: "2.0", id: "non-numeric", result: null });
    child.send({ jsonrpc: "2.0", id: 9999, result: null });
    child.send({ jsonrpc: "2.0", id: { not: "scalar" }, method: "x" });
    expect(errors.some((e) => e.reason.includes("response id"))).toBe(true);
    expect(errors.some((e) => e.reason.includes("no pending request"))).toBe(true);
    expect(errors.some((e) => e.reason.includes("id field must be"))).toBe(true);
    p.dispose();
  });

  it("normalizes a missing `result` field on a response to null", async () => {
    const { parent, child } = makeChannelPair();
    const p = new IpcJsonRpcTransport(parent);
    const c = new IpcJsonRpcTransport(child);
    c.onRequest((req) => {
      child.send({ jsonrpc: "2.0", id: req.id, result: undefined });
    });
    expect(await p.request("nullish")).toBeNull();
    p.dispose();
    c.dispose();
  });

  it("falls back to a generic remote-error when error envelope is malformed", async () => {
    const { parent, child } = makeChannelPair();
    const p = new IpcJsonRpcTransport(parent);
    const c = new IpcJsonRpcTransport(child);
    c.onRequest((req) => {
      child.send({ jsonrpc: "2.0", id: req.id, error: { weird: true } });
    });
    const err = await p.request("malformed").catch((e: unknown) => e);
    expect((err as JsonRpcRemoteError).code).toBe(0);
    expect((err as JsonRpcRemoteError).message).toBe("remote error");
    p.dispose();
    c.dispose();
  });
});

describe("ipcChannelFromProcess", () => {
  it("delegates send/on/off to the underlying process", () => {
    const emitter = new EventEmitter();
    const sent: unknown[] = [];
    const proc = Object.assign(emitter, {
      send: (msg: unknown): boolean => {
        sent.push(msg);
        return true;
      },
    }) as unknown as NodeJS.Process;
    const channel = ipcChannelFromProcess(proc);
    channel.send({ hello: "world" });
    expect(sent).toEqual([{ hello: "world" }]);
    let received: unknown;
    const handler = (m: unknown): void => void (received = m);
    channel.onMessage(handler);
    emitter.emit("message", { reply: 1 });
    expect(received).toEqual({ reply: 1 });
    received = undefined;
    channel.offMessage(handler);
    emitter.emit("message", { reply: 2 });
    expect(received).toBeUndefined();
  });

  it("throws when the process has no IPC channel", () => {
    const stub = new EventEmitter() as unknown as NodeJS.Process;
    expect(() => ipcChannelFromProcess(stub)).toThrow(/process\.send is undefined/);
  });
});

describe("spawnWorker — additional coverage", () => {
  it("init timeout → SIGKILL and rejects", async () => {
    const tmpDir = mkdtempSync(`${tmpdir()}/worker-spawn-test-`);
    const hangingPath = resolve(tmpDir, "hanging-worker.mjs");
    writeFileSync(hangingPath, "setInterval(() => {}, 1000);\n");
    await expect(
      spawnWorker(EMPTY_CAPS, { bootstrapPath: hangingPath, readyTimeoutMs: 100 }),
    ).rejects.toThrow(/did not become ready/);
  });

  it("rejects when fork target exits before ready", async () => {
    const indexPath = resolve(HARNESS_ROOT, "dist/index.js");
    await expect(
      spawnWorker(EMPTY_CAPS, { bootstrapPath: indexPath, readyTimeoutMs: 200 }),
    ).rejects.toThrow(/did not become ready|exited before ready/);
  });

  it("rejects with bootstrap's `{kind:'error'}` for invalid capabilities", async () => {
    const badCaps = {
      fs: { read: [], write: [] },
      net: { allow: [] },
      env: { allow: [] },
      proc: { spawn: "yes" },
    } as unknown as CapabilityDeclaration;
    await expect(
      spawnWorker(badCaps, { bootstrapPath: BOOTSTRAP_PATH, readyTimeoutMs: 2000 }),
    ).rejects.toThrow(/capability declaration invalid/);
  });

  it("uses default bootstrap path when omitted (covers defaultBootstrapPath branch)", async () => {
    // Under vitest, default URL resolves to src/worker/bootstrap.js
    // which does not exist on disk; either ENOENT or the timeout is
    // acceptable proof the branch ran.
    const result = await spawnWorker(EMPTY_CAPS, { readyTimeoutMs: 200 }).catch(
      (e: unknown) => e as Error,
    );
    expect(result).toBeInstanceOf(Error);
    expect((result as Error).message).toMatch(
      /ENOENT|Cannot find module|did not become ready|exited before ready/,
    );
  });

  it("terminate() resolves immediately if the worker already exited", async () => {
    const worker = await spawnWorker(EMPTY_CAPS, {
      bootstrapPath: BOOTSTRAP_PATH,
      crashOnInit: true,
    });
    await new Promise<void>((res) => setTimeout(res, 200));
    const start = Date.now();
    await worker.terminate();
    expect(Date.now() - start).toBeLessThan(200);
  });

  it("notify is best-effort and survives terminate", async () => {
    const worker = await spawnWorker(EMPTY_CAPS, { bootstrapPath: BOOTSTRAP_PATH });
    worker.notify("log", { msg: "before" });
    await worker.terminate();
    expect(() => {
      worker.notify("log", { msg: "after" });
    }).not.toThrow();
  });

  it("terminate() falls back to SIGKILL when the worker stays alive past disconnect", async () => {
    const tmpDir = mkdtempSync(`${tmpdir()}/worker-spawn-test-`);
    const stubPath = resolve(tmpDir, "stubborn-worker.mjs");
    writeFileSync(
      stubPath,
      [
        "process.on('message', m => { if (m && m.kind === 'init') process.send({kind:'ready'}); });",
        "setInterval(() => {}, 1000);",
      ].join("\n"),
    );
    const worker = await spawnWorker(EMPTY_CAPS, {
      bootstrapPath: stubPath,
      readyTimeoutMs: 2000,
    });
    const start = Date.now();
    await worker.terminate();
    const elapsed = Date.now() - start;
    expect(elapsed).toBeGreaterThanOrEqual(900);
    expect(elapsed).toBeLessThan(2500);
  }, 10000);
});

describe("spawnWorker — exit-handler handoff race (Comment 3)", () => {
  it("emits crash even when child exits immediately after ready ack", async () => {
    // The worker sends ready then exits in the same tick. The stateful
    // single-listener approach must observe the exit without a gap.
    const tmpDir = mkdtempSync(`${tmpdir()}/worker-spawn-test-`);
    const stubPath = resolve(tmpDir, "instant-exit-after-ready.mjs");
    writeFileSync(
      stubPath,
      [
        "process.on('message', m => {",
        "  if (m && m.kind === 'init') {",
        "    process.send({kind:'ready'});",
        "    process.exit(42);",
        "  }",
        "});",
      ].join("\n"),
    );
    const worker = await spawnWorker(EMPTY_CAPS, {
      bootstrapPath: stubPath,
      readyTimeoutMs: 2000,
    });
    const events: WorkerCrashEvent[] = [];
    worker.on("crash", (e) => events.push(e));
    await new Promise<void>((res) => setTimeout(res, 300));
    expect(events).toHaveLength(1);
    expect(events[0]?.exitCode).toBe(42);
  });
});

describe("buildCrashEvent", () => {
  it("returns the bare sentinel when both code and signal are null", () => {
    expect(buildCrashEvent(null, null)).toEqual({ code: ToolErrorCode.ToolWorkerCrashed });
  });
  it("includes only exitCode when signal is null", () => {
    expect(buildCrashEvent(2, null)).toEqual({
      code: ToolErrorCode.ToolWorkerCrashed,
      exitCode: 2,
    });
  });
  it("includes only signal when code is null", () => {
    expect(buildCrashEvent(null, "SIGTERM")).toEqual({
      code: ToolErrorCode.ToolWorkerCrashed,
      signal: "SIGTERM",
    });
  });
  it("includes both when both are present", () => {
    expect(buildCrashEvent(0, "SIGINT")).toEqual({
      code: ToolErrorCode.ToolWorkerCrashed,
      exitCode: 0,
      signal: "SIGINT",
    });
  });
});

describe("runBootstrap (in-process driver)", () => {
  function makeStubProcess(): {
    proc: NodeJS.Process;
    sent: unknown[];
    inbound(message: unknown): void;
  } {
    const sent: unknown[] = [];
    const handlers = new Map<string, Set<(arg: unknown) => void>>();
    const stub = {
      send: (m: unknown): boolean => {
        sent.push(m);
        return true;
      },
      on: (event: string, h: (a: unknown) => void): unknown => {
        let set = handlers.get(event);
        if (set === undefined) {
          set = new Set();
          handlers.set(event, set);
        }
        set.add(h);
        return stub;
      },
      off: (event: string, h: (a: unknown) => void): unknown => {
        handlers.get(event)?.delete(h);
        return stub;
      },
      exit: (): never => {
        throw new Error("__stub_exit__");
      },
    } as unknown as NodeJS.Process;
    return {
      proc: stub,
      sent,
      inbound: (m) => {
        for (const h of [...(handlers.get("message") ?? [])]) h(m);
      },
    };
  }

  it("validates, freezes, and replies with ready", () => {
    const stub = makeStubProcess();
    runBootstrap(stub.proc);
    stub.inbound({ kind: "init", capabilities: EMPTY_CAPS });
    expect(stub.sent).toContainEqual({ kind: "ready" });
  });

  it("ping → 'pong' after init", () => {
    const stub = makeStubProcess();
    runBootstrap(stub.proc);
    stub.inbound({ kind: "init", capabilities: EMPTY_CAPS });
    stub.sent.length = 0;
    stub.inbound({ jsonrpc: "2.0", id: 1, method: "ping" });
    expect(stub.sent).toContainEqual({ jsonrpc: "2.0", id: 1, result: "pong" });
  });

  it("returns MethodNotFound for unknown request methods", () => {
    const stub = makeStubProcess();
    runBootstrap(stub.proc);
    stub.inbound({ kind: "init", capabilities: EMPTY_CAPS });
    stub.sent.length = 0;
    stub.inbound({ jsonrpc: "2.0", id: 7, method: "nope" });
    expect(stub.sent).toContainEqual(
      expect.objectContaining({
        jsonrpc: "2.0",
        id: 7,
        error: expect.objectContaining({ code: -32601 }) as unknown,
      }),
    );
  });

  it("ignores the `log` and unknown notifications silently", () => {
    const stub = makeStubProcess();
    runBootstrap(stub.proc);
    stub.inbound({ kind: "init", capabilities: EMPTY_CAPS });
    stub.sent.length = 0;
    stub.inbound({ jsonrpc: "2.0", method: "log", params: { msg: "x" } });
    stub.inbound({ jsonrpc: "2.0", method: "unknown" });
    expect(stub.sent).toEqual([]);
  });

  it("rejects non-init first message and signals exit(1)", () => {
    const stub = makeStubProcess();
    runBootstrap(stub.proc);
    expect(() => {
      stub.inbound("not an init envelope");
    }).toThrow(/__stub_exit__/);
    expect(stub.sent[0]).toEqual({
      kind: "error",
      message: expect.stringContaining("first IPC message") as unknown,
    });
  });

  it("rejects invalid capability declaration and signals exit(1)", () => {
    const stub = makeStubProcess();
    runBootstrap(stub.proc);
    expect(() => {
      stub.inbound({ kind: "init", capabilities: { wrong: true } });
    }).toThrow(/__stub_exit__/);
    expect((stub.sent[0] as { message: string }).message).toMatch(/capability declaration invalid/);
  });

  it("supports the crash-on-init test hook by exiting after ready", () => {
    const stub = makeStubProcess();
    runBootstrap(stub.proc);
    expect(() => {
      stub.inbound({ kind: "init", capabilities: EMPTY_CAPS, crashOnInit: true });
    }).toThrow(/__stub_exit__/);
    expect(stub.sent).toContainEqual({ kind: "ready" });
  });
});

/** Back-to-back in-memory IPC channel pair for transport unit tests. */
function makeChannelPair(): { parent: IpcChannel; child: IpcChannel } {
  const parentHandlers = new Set<(msg: unknown) => void>();
  const childHandlers = new Set<(msg: unknown) => void>();
  return {
    parent: {
      send: (msg) => {
        for (const h of [...childHandlers]) h(msg);
      },
      onMessage: (h) => parentHandlers.add(h),
      offMessage: (h) => parentHandlers.delete(h),
    },
    child: {
      send: (msg) => {
        for (const h of [...parentHandlers]) h(msg);
      },
      onMessage: (h) => childHandlers.add(h),
      offMessage: (h) => childHandlers.delete(h),
    },
  };
}
