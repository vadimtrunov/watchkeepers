/**
 * `invokeTool` worker-process path tests (M5.3.b.b.d).
 *
 * Covers the dispatcher branch added by this milestone:
 *   - routing: `tool.kind === 'worker'` reaches the new handler;
 *   - happy path: end-to-end `echo` round-trip via a real fixture worker;
 *   - sync-deny: `requiredOps` violations reject with -32003 BEFORE spawn;
 *   - crash translation: `process.exit(1)` mid-call surfaces as -32004;
 *   - terminate: no orphan child after a successful invocation.
 *
 * The fixture worker is materialised on disk per test run so the suite
 * stays self-contained — adding a permanent file under `test/fixtures/`
 * would invite drift with the bootstrap module that the production code
 * already validates.
 */

import { execSync } from "node:child_process";
import { existsSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { afterAll, beforeAll, describe, expect, it } from "vitest";

import type { CapabilityDeclaration } from "../src/capabilities.js";
import { handleLine } from "../src/dispatcher.js";
import { ToolErrorCode, invokeToolHandler } from "../src/invokeTool.js";
import { createDefaultRegistry, type ShutdownSignal } from "../src/methods.js";
import { JSON_RPC_VERSION, JsonRpcErrorCode, type JsonRpcValue } from "../src/types.js";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const HARNESS_ROOT = resolve(__dirname, "..");
const FIXTURE_DIR = mkdtempSync(`${tmpdir()}/invokeTool-worker-fixture-`);
const FIXTURE_PATH = resolve(FIXTURE_DIR, "echo-worker.mjs");

const EMPTY_CAPS: CapabilityDeclaration = {
  fs: { read: [], write: [] },
  net: { allow: [] },
  env: { allow: [] },
  proc: { spawn: false },
};

const ALLOW_FS_READ_TMP_CAPS: CapabilityDeclaration = {
  fs: { read: ["/tmp/allowed.txt"], write: [] },
  net: { allow: [] },
  env: { allow: [] },
  proc: { spawn: false },
};

beforeAll(() => {
  // Production worker dispatcher needs the built bootstrap module on disk
  // (the runtime path resolves dist/worker/bootstrap.js); the spawn-worker
  // suite shares this build, so a no-op here when dist is already present.
  const builtBootstrap = resolve(HARNESS_ROOT, "dist/worker/bootstrap.js");
  if (!existsSync(builtBootstrap)) {
    execSync("pnpm build", { cwd: HARNESS_ROOT, stdio: "inherit" });
  }

  // Materialise a tiny fixture worker that handles `echo` and `crash`.
  // Inline-source on disk keeps the fixture co-located with the suite
  // and avoids polluting test/fixtures/ with a permanent shim.
  writeFileSync(
    FIXTURE_PATH,
    [
      "// Fixture worker for invokeTool-worker.test.ts.",
      "// Mirrors bootstrap's init handshake, then handles 'echo' and 'crash'.",
      "process.on('message', (msg) => {",
      "  if (msg && msg.kind === 'init') {",
      "    process.send({ kind: 'ready' });",
      "    process.on('message', (m) => {",
      "      if (!m || m.jsonrpc !== '2.0' || typeof m.id === 'undefined') return;",
      "      if (m.method === 'echo') {",
      "        process.send({ jsonrpc: '2.0', id: m.id, result: m.params });",
      "        return;",
      "      }",
      "      if (m.method === 'crash') {",
      "        process.exit(1);",
      "      }",
      "      if (m.method === 'capDenied') {",
      "        process.send({",
      "          jsonrpc: '2.0',",
      "          id: m.id,",
      "          error: { code: -32003, message: 'remote: cap denied', data: { axis: 'fs.read' } },",
      "        });",
      "        return;",
      "      }",
      "      if (m.method === 'genericError') {",
      "        process.send({",
      "          jsonrpc: '2.0',",
      "          id: m.id,",
      "          error: { code: -32000, message: 'remote boom' },",
      "        });",
      "        return;",
      "      }",
      "      process.send({",
      "        jsonrpc: '2.0',",
      "        id: m.id,",
      "        error: { code: -32601, message: 'method not found: ' + m.method },",
      "      });",
      "    });",
      "  }",
      "});",
    ].join("\n"),
  );
});

afterAll(() => {
  // Best-effort: the OS cleans /tmp eventually; avoid noisy rm failures.
});

interface JsonRpcSuccessLine {
  jsonrpc: string;
  id: string | number | null;
  result: { output: JsonRpcValue };
}
interface JsonRpcErrorLine {
  jsonrpc: string;
  id: string | number | null;
  error: { code: number; message: string; data?: JsonRpcValue };
}

function decodeLine(line: string | undefined): unknown {
  if (line === undefined) throw new Error("expected a response line");
  expect(line.endsWith("\n")).toBe(true);
  return JSON.parse(line.slice(0, -1));
}

function workerParams(
  method: string,
  input: JsonRpcValue,
  caps: CapabilityDeclaration = EMPTY_CAPS,
  requiredOps?: readonly JsonRpcValue[],
  spawnOptions: Record<string, unknown> = { bootstrapPath: FIXTURE_PATH },
): JsonRpcValue {
  const tool: Record<string, JsonRpcValue> = {
    kind: "worker",
    method,
    capabilities: caps,
    spawnOptions: spawnOptions as unknown as JsonRpcValue,
  };
  if (requiredOps !== undefined) tool.requiredOps = requiredOps;
  return { tool, input };
}

describe("invokeTool — worker happy path", () => {
  it("round-trips echo via a real forked fixture worker", async () => {
    const out = await invokeToolHandler(workerParams("echo", { x: 1, nested: [2, 3] }));
    expect(out).toEqual({ output: { x: 1, nested: [2, 3] } });
  });

  it("preserves primitive inputs through the worker round-trip", async () => {
    const out = await invokeToolHandler(workerParams("echo", "hello"));
    expect(out).toEqual({ output: "hello" });
  });

  it("does not leak a child process after the call settles", async () => {
    // Pre-count node children of this process so we can compare after
    // the invocation completes. `pgrep -P <pid>` returns lines; a leak
    // would show up as a delta after the awaited promise resolves.
    const pid = process.pid;
    const before = execSync(`pgrep -P ${String(pid)} || true`)
      .toString()
      .trim()
      .split("\n").length;
    await invokeToolHandler(workerParams("echo", { ok: true }));
    // Give the fork's exit a tick to be reaped.
    await new Promise<void>((res) => setTimeout(res, 100));
    const after = execSync(`pgrep -P ${String(pid)} || true`)
      .toString()
      .trim()
      .split("\n").length;
    expect(after).toBeLessThanOrEqual(before);
  });
});

describe("invokeTool — worker sync deny (AC3)", () => {
  it("rejects fs.write outside the declared allowlist BEFORE spawning", async () => {
    // proc.spawn=false + requiredOps=[fs.write '/tmp/secret']: the gate
    // denies, the dispatcher MUST short-circuit before fork() runs.
    // An invalid bootstrap path proves the worker was never spawned —
    // a real spawn would error with ENOENT instead of the deny code.
    const params = workerParams(
      "echo",
      null,
      EMPTY_CAPS,
      [{ kind: "fs.write", path: "/tmp/secret" }],
      { bootstrapPath: "/this/path/does/not/exist.mjs" },
    );
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      name: "MethodError",
      code: ToolErrorCode.ToolCapabilityDenied,
      message: expect.stringContaining("/tmp/secret") as unknown,
    });
  });

  it("denies proc.spawn synchronously when declaration disallows children", async () => {
    const params = workerParams("echo", null, EMPTY_CAPS, [{ kind: "proc.spawn" }], {
      bootstrapPath: "/this/path/does/not/exist.mjs",
    });
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: ToolErrorCode.ToolCapabilityDenied,
      message: expect.stringContaining("proc.spawn") as unknown,
    });
  });

  it("allows fs.read inside the declared allowlist (gate passes through)", async () => {
    const params = workerParams("echo", { tag: "ok" }, ALLOW_FS_READ_TMP_CAPS, [
      { kind: "fs.read", path: "/tmp/allowed.txt" },
    ]);
    const out = await invokeToolHandler(params);
    expect(out).toEqual({ output: { tag: "ok" } });
  });

  it("denies fs.read outside the declared allowlist with a useful reason", async () => {
    const params = workerParams(
      "echo",
      null,
      ALLOW_FS_READ_TMP_CAPS,
      [{ kind: "fs.read", path: "/tmp/other.txt" }],
      { bootstrapPath: "/nope.mjs" },
    );
    const err = await invokeToolHandler(params).catch((e: unknown) => e);
    expect(err).toMatchObject({
      code: ToolErrorCode.ToolCapabilityDenied,
      data: { reason: expect.stringContaining("/tmp/other.txt") as unknown },
    });
  });
});

describe("invokeTool — worker crash translation (AC4)", () => {
  it("surfaces process.exit(1) mid-invocation as ToolWorkerCrashed (-32004)", async () => {
    const params = workerParams("crash", null);
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      name: "MethodError",
      code: ToolErrorCode.ToolWorkerCrashed,
    });
  });
});

describe("invokeTool — worker remote-error translation", () => {
  it("preserves remote -32003 envelopes as ToolCapabilityDenied with data", async () => {
    const params = workerParams("capDenied", null);
    const err = await invokeToolHandler(params).catch((e: unknown) => e);
    expect(err).toMatchObject({
      code: ToolErrorCode.ToolCapabilityDenied,
      data: { axis: "fs.read" },
    });
  });

  it("translates a generic remote error to ToolExecutionError", async () => {
    const params = workerParams("genericError", null);
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: ToolErrorCode.ToolExecutionError,
      message: expect.stringContaining("remote boom") as unknown,
    });
  });

  it("translates a remote MethodNotFound through as ToolExecutionError", async () => {
    const params = workerParams("nope", null);
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: ToolErrorCode.ToolExecutionError,
    });
  });
});

describe("invokeTool — dispatcher routing of tool.kind === 'worker' (AC1, AC6a)", () => {
  it("routes a worker-tool JSON-RPC request through handleLine end-to-end", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const request = JSON.stringify({
      jsonrpc: JSON_RPC_VERSION,
      id: 100,
      method: "invokeTool",
      params: workerParams("echo", { hello: "world" }),
    });

    const line = await handleLine(registry, request);
    const decoded = decodeLine(line) as JsonRpcSuccessLine;
    expect(decoded.id).toBe(100);
    expect(decoded.result).toEqual({ output: { hello: "world" } });
  });

  it("surfaces sync-deny through the dispatcher error envelope (-32003)", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const request = JSON.stringify({
      jsonrpc: JSON_RPC_VERSION,
      id: 101,
      method: "invokeTool",
      params: workerParams("echo", null, EMPTY_CAPS, [{ kind: "fs.write", path: "/etc/passwd" }], {
        bootstrapPath: "/nope.mjs",
      }),
    });

    const line = await handleLine(registry, request);
    const decoded = decodeLine(line) as JsonRpcErrorLine;
    expect(decoded.id).toBe(101);
    expect(decoded.error.code).toBe(ToolErrorCode.ToolCapabilityDenied);
    expect(decoded.error.message).toContain("/etc/passwd");
  });

  it("surfaces a worker crash through the dispatcher error envelope (-32004)", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const request = JSON.stringify({
      jsonrpc: JSON_RPC_VERSION,
      id: 102,
      method: "invokeTool",
      params: workerParams("crash", null),
    });

    const line = await handleLine(registry, request);
    const decoded = decodeLine(line) as JsonRpcErrorLine;
    expect(decoded.id).toBe(102);
    expect(decoded.error.code).toBe(ToolErrorCode.ToolWorkerCrashed);
  });
});

describe("invokeTool — worker spawn failure / message branches", () => {
  it("translates a spawnWorker failure to ToolExecutionError", async () => {
    const params = workerParams("echo", null, EMPTY_CAPS, undefined, {
      bootstrapPath: "/no/such/path/missing.mjs",
      readyTimeoutMs: 200,
    });
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: ToolErrorCode.ToolExecutionError,
    });
  });

  it("translates a SIGTERM-only crash signal into a useful message", async () => {
    // Materialise a fixture worker that responds to 'sigself' by sending
    // SIGTERM to itself; spawn.ts surfaces the resulting exit as a crash
    // event with `signal` populated and `exitCode` null.
    const sigPath = resolve(FIXTURE_DIR, "sig-worker.mjs");
    writeFileSync(
      sigPath,
      [
        "process.on('message', (msg) => {",
        "  if (msg && msg.kind === 'init') {",
        "    process.send({ kind: 'ready' });",
        "    process.on('message', (m) => {",
        "      if (m && m.method === 'sigself') process.kill(process.pid, 'SIGTERM');",
        "    });",
        "  }",
        "});",
      ].join("\n"),
    );
    const params = workerParams("sigself", null, EMPTY_CAPS, undefined, {
      bootstrapPath: sigPath,
    });
    const err = await invokeToolHandler(params).catch((e: unknown) => e);
    expect(err).toMatchObject({
      code: ToolErrorCode.ToolWorkerCrashed,
      message: expect.stringContaining("signal=SIGTERM") as unknown,
    });
  });
});

describe("invokeTool — worker remote-error -32004 translation", () => {
  it("preserves a remote -32004 envelope as ToolWorkerCrashed (no actual crash)", async () => {
    const remoteCrashPath = resolve(FIXTURE_DIR, "remote-crash-worker.mjs");
    writeFileSync(
      remoteCrashPath,
      [
        "process.on('message', (msg) => {",
        "  if (msg && msg.kind === 'init') {",
        "    process.send({ kind: 'ready' });",
        "    process.on('message', (m) => {",
        "      if (m && m.method === 'remoteCrash') {",
        "        process.send({",
        "          jsonrpc: '2.0',",
        "          id: m.id,",
        "          error: { code: -32004, message: 'remote: simulated crash report' },",
        "        });",
        "      }",
        "    });",
        "  }",
        "});",
      ].join("\n"),
    );
    const params = workerParams("remoteCrash", null, EMPTY_CAPS, undefined, {
      bootstrapPath: remoteCrashPath,
    });
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: ToolErrorCode.ToolWorkerCrashed,
      message: expect.stringContaining("simulated crash report") as unknown,
    });
  });
});

describe("invokeTool — worker requiredOps validation (extra branches)", () => {
  it("accepts a valid net.connect requiredOp and routes through happy path", async () => {
    const caps: CapabilityDeclaration = {
      fs: { read: [], write: [] },
      net: { allow: ["example.com:443"] },
      env: { allow: [] },
      proc: { spawn: false },
    };
    const params = workerParams("echo", { ok: 1 }, caps, [
      { kind: "net.connect", host: "example.com", port: 443 },
    ]);
    await expect(invokeToolHandler(params)).resolves.toEqual({ output: { ok: 1 } });
  });

  it("accepts a port-less net.connect requiredOp", async () => {
    const caps: CapabilityDeclaration = {
      fs: { read: [], write: [] },
      net: { allow: ["raw.example.com"] },
      env: { allow: [] },
      proc: { spawn: false },
    };
    const params = workerParams("echo", { ok: 1 }, caps, [
      { kind: "net.connect", host: "raw.example.com" },
    ]);
    await expect(invokeToolHandler(params)).resolves.toEqual({ output: { ok: 1 } });
  });

  it("syncs-denies even when spawnOptions field is omitted entirely", async () => {
    // Exercises the `spawnOptions === undefined` branch in validateTool.
    // The deny short-circuits before any default-bootstrap-path fork.
    const params: JsonRpcValue = {
      tool: {
        kind: "worker",
        method: "echo",
        capabilities: EMPTY_CAPS as unknown as JsonRpcValue,
        requiredOps: [{ kind: "fs.write", path: "/etc/passwd" }],
      },
      input: null,
    };
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: ToolErrorCode.ToolCapabilityDenied,
    });
  });

  it("accepts a worker tool with no requiredOps and no spawnOptions (validates the both-undefined branch)", async () => {
    // Uses the default bootstrap which has no `echo` handler → expect a
    // remote MethodNotFound translated to ToolExecutionError. The point
    // here is to drive the validateTool branch where requiredOps is
    // undefined AND spawnOptions is undefined.
    const params: JsonRpcValue = {
      tool: {
        kind: "worker",
        method: "ping",
        capabilities: EMPTY_CAPS as unknown as JsonRpcValue,
      },
      input: null,
    };
    // The default bootstrap path under vitest resolves to src/worker/...
    // which does not exist; spawnWorker rejects → ToolExecutionError.
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: ToolErrorCode.ToolExecutionError,
    });
  });

  it("accepts a valid env.get requiredOp", async () => {
    const caps: CapabilityDeclaration = {
      fs: { read: [], write: [] },
      net: { allow: [] },
      env: { allow: ["HOME"] },
      proc: { spawn: false },
    };
    const params = workerParams("echo", { ok: 1 }, caps, [{ kind: "env.get", name: "HOME" }]);
    await expect(invokeToolHandler(params)).resolves.toEqual({ output: { ok: 1 } });
  });

  it("rejects a net.connect requiredOp with non-string host", async () => {
    const params = workerParams("echo", null, EMPTY_CAPS, [{ kind: "net.connect", host: 80 }]);
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
      message: expect.stringContaining("host") as unknown,
    });
  });

  it("rejects a net.connect requiredOp with non-numeric port", async () => {
    const params = workerParams("echo", null, EMPTY_CAPS, [
      { kind: "net.connect", host: "h", port: "80" },
    ]);
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
      message: expect.stringContaining("port") as unknown,
    });
  });

  it("rejects an env.get requiredOp with non-string name", async () => {
    const params = workerParams("echo", null, EMPTY_CAPS, [{ kind: "env.get", name: 1 }]);
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
      message: expect.stringContaining("name") as unknown,
    });
  });
});

describe("invokeTool — worker InvalidParams (AC6)", () => {
  it("rejects worker tool with non-string method", async () => {
    const params: JsonRpcValue = {
      tool: {
        kind: "worker",
        method: 123 as unknown as string,
        capabilities: EMPTY_CAPS as unknown as JsonRpcValue,
      },
      input: null,
    };
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
      message: expect.stringContaining("method") as unknown,
    });
  });

  it("rejects worker tool with empty-string method", async () => {
    const params: JsonRpcValue = {
      tool: {
        kind: "worker",
        method: "",
        capabilities: EMPTY_CAPS as unknown as JsonRpcValue,
      },
      input: null,
    };
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects worker tool with malformed capabilities", async () => {
    const params: JsonRpcValue = {
      tool: {
        kind: "worker",
        method: "echo",
        capabilities: { fs: { read: [], write: [] } } as unknown as JsonRpcValue,
      },
      input: null,
    };
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
      message: expect.stringContaining("capabilities") as unknown,
    });
  });

  it("rejects worker tool with non-array requiredOps", async () => {
    const params: JsonRpcValue = {
      tool: {
        kind: "worker",
        method: "echo",
        capabilities: EMPTY_CAPS as unknown as JsonRpcValue,
        requiredOps: { not: "an array" } as unknown as JsonRpcValue,
      },
      input: null,
    };
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects an unknown requiredOps[i].kind", async () => {
    const params: JsonRpcValue = workerParams("echo", null, EMPTY_CAPS, [
      { kind: "fs.delete", path: "/tmp/x" },
    ]);
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects requiredOps[i] missing the required path field", async () => {
    const params: JsonRpcValue = workerParams("echo", null, EMPTY_CAPS, [{ kind: "fs.read" }]);
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects requiredOps[i] that is not an object", async () => {
    const params: JsonRpcValue = workerParams("echo", null, EMPTY_CAPS, ["not-an-object"]);
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });
});
