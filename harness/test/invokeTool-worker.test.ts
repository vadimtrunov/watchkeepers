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

import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from "vitest";

import type { CapabilityDeclaration } from "../src/capabilities.js";
import { handleLine } from "../src/dispatcher.js";
import {
  ToolErrorCode,
  invokeToolHandler,
  runWorkerTool,
  type WorkerTool,
} from "../src/invokeTool.js";
import { __resetActiveToolsetForTests, setActiveToolset } from "../src/manifest.js";
import { createDefaultRegistry, type ShutdownSignal } from "../src/methods.js";
import { JSON_RPC_VERSION, JsonRpcErrorCode, type JsonRpcValue } from "../src/types.js";
import type { SpawnWorkerOptions } from "../src/worker/spawn.js";

// M5.5.b.a manifest ACL gate: worker tools resolve to `tool.method`
// when no explicit `name` is supplied. The fixture worker handles
// `echo`, `crash`, `capDenied`, `genericError`, `sigself`, `remoteCrash`,
// `ping`; pre-seed the active toolset so every fixture method dispatches
// past the gate. Tests that exercise pre-spawn validation/InvalidParams
// behaviour also pass through the gate first — the seed list keeps
// those paths reachable without per-test setup.
const ACL_FIXTURE_METHODS: readonly string[] = [
  "echo",
  "crash",
  "capDenied",
  "genericError",
  "nope",
  "sigself",
  "remoteCrash",
  "ping",
];
beforeEach(() => {
  __resetActiveToolsetForTests();
  setActiveToolset(ACL_FIXTURE_METHODS);
});
afterEach(() => {
  __resetActiveToolsetForTests();
});

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

/**
 * Wire-shape params builder — what a JSON-RPC client sends. NEVER carries
 * `spawnOptions`; that field is rejected by validateTool (B1 fix). Use
 * {@link callWorker} instead when the test needs a fixture worker.
 */
function workerParams(
  method: string,
  input: JsonRpcValue,
  caps: CapabilityDeclaration = EMPTY_CAPS,
  requiredOps?: readonly JsonRpcValue[],
): JsonRpcValue {
  const tool: Record<string, JsonRpcValue> = {
    kind: "worker",
    method,
    capabilities: caps,
  };
  if (requiredOps !== undefined) tool.requiredOps = requiredOps;
  return { tool, input };
}

/**
 * Internal-API helper for tests that DO need a fixture worker (happy path,
 * crash, remote-error translation, signal-only crash). Goes through
 * {@link runWorkerTool} so it can pass `bootstrapPath` via the typed
 * internal seam — exactly the path wire callers cannot reach.
 */
function callWorker(
  method: string,
  input: JsonRpcValue,
  caps: CapabilityDeclaration = EMPTY_CAPS,
  requiredOps?: readonly ToolOperationInput[],
  spawnOptions: SpawnWorkerOptions = { bootstrapPath: FIXTURE_PATH },
): Promise<JsonRpcValue> {
  const tool: WorkerTool =
    requiredOps === undefined
      ? { kind: "worker", method, capabilities: caps }
      : { kind: "worker", method, capabilities: caps, requiredOps };
  return runWorkerTool(tool, input, spawnOptions);
}

// Loose alias for ToolOperation literals used in the test fixtures —
// keeps the call sites in this file readable without re-importing the
// production union type.
type ToolOperationInput =
  | { kind: "fs.read"; path: string }
  | { kind: "fs.write"; path: string }
  | { kind: "net.connect"; host: string; port?: number }
  | { kind: "env.get"; name: string }
  | { kind: "proc.spawn" };

describe("invokeTool — worker happy path", () => {
  it("round-trips echo via a real forked fixture worker", async () => {
    const out = await callWorker("echo", { x: 1, nested: [2, 3] });
    expect(out).toEqual({ x: 1, nested: [2, 3] });
  });

  it("preserves primitive inputs through the worker round-trip", async () => {
    const out = await callWorker("echo", "hello");
    expect(out).toEqual("hello");
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
    await callWorker("echo", { ok: true });
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
    // denies; runWorkerTool MUST short-circuit before fork() runs. With
    // bootstrapPath pointed at a non-existent file, a real spawn would
    // surface as ENOENT (ToolExecutionError) — getting -32003 back proves
    // the deny landed before spawnWorker was invoked.
    await expect(
      callWorker("echo", null, EMPTY_CAPS, [{ kind: "fs.write", path: "/tmp/secret" }], {
        bootstrapPath: "/this/path/does/not/exist.mjs",
      }),
    ).rejects.toMatchObject({
      name: "MethodError",
      code: ToolErrorCode.ToolCapabilityDenied,
      message: expect.stringContaining("/tmp/secret") as unknown,
    });
  });

  it("denies proc.spawn synchronously when declaration disallows children", async () => {
    await expect(
      callWorker("echo", null, EMPTY_CAPS, [{ kind: "proc.spawn" }], {
        bootstrapPath: "/this/path/does/not/exist.mjs",
      }),
    ).rejects.toMatchObject({
      code: ToolErrorCode.ToolCapabilityDenied,
      message: expect.stringContaining("proc.spawn") as unknown,
    });
  });

  it("allows fs.read inside the declared allowlist (gate passes through)", async () => {
    const out = await callWorker("echo", { tag: "ok" }, ALLOW_FS_READ_TMP_CAPS, [
      { kind: "fs.read", path: "/tmp/allowed.txt" },
    ]);
    expect(out).toEqual({ tag: "ok" });
  });

  it("denies fs.read outside the declared allowlist with a useful reason", async () => {
    const err = await callWorker(
      "echo",
      null,
      ALLOW_FS_READ_TMP_CAPS,
      [{ kind: "fs.read", path: "/tmp/other.txt" }],
      { bootstrapPath: "/nope.mjs" },
    ).catch((e: unknown) => e);
    expect(err).toMatchObject({
      code: ToolErrorCode.ToolCapabilityDenied,
      data: { reason: expect.stringContaining("/tmp/other.txt") as unknown },
    });
  });
});

describe("invokeTool — worker crash translation (AC4)", () => {
  it("surfaces process.exit(1) mid-invocation as ToolWorkerCrashed (-32004)", async () => {
    await expect(callWorker("crash", null)).rejects.toMatchObject({
      name: "MethodError",
      code: ToolErrorCode.ToolWorkerCrashed,
    });
  });
});

describe("invokeTool — worker remote-error translation", () => {
  it("preserves remote -32003 envelopes as ToolCapabilityDenied with data", async () => {
    const err = await callWorker("capDenied", null).catch((e: unknown) => e);
    expect(err).toMatchObject({
      code: ToolErrorCode.ToolCapabilityDenied,
      data: { axis: "fs.read" },
    });
  });

  it("translates a generic remote error to ToolExecutionError", async () => {
    await expect(callWorker("genericError", null)).rejects.toMatchObject({
      code: ToolErrorCode.ToolExecutionError,
      message: expect.stringContaining("remote boom") as unknown,
    });
  });

  it("translates a remote MethodNotFound through as ToolExecutionError", async () => {
    await expect(callWorker("nope", null)).rejects.toMatchObject({
      code: ToolErrorCode.ToolExecutionError,
    });
  });
});

describe("invokeTool — dispatcher routing of tool.kind === 'worker' (AC1, AC6a)", () => {
  it("routes a worker-tool JSON-RPC request through handleLine into the worker branch", async () => {
    // Post-B1 the wire boundary cannot pick a bootstrap module, so a wire
    // happy-path round-trip via fixture is structurally impossible — that
    // IS the security feature. Prove the worker branch is reached by
    // surfacing a sync-deny envelope (only the worker branch produces
    // -32003 / ToolCapabilityDenied), with a `requiredOps` payload that
    // exercises validateTool's worker leg end-to-end.
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const request = JSON.stringify({
      jsonrpc: JSON_RPC_VERSION,
      id: 100,
      method: "invokeTool",
      params: workerParams("echo", { hello: "world" }, EMPTY_CAPS, [
        { kind: "fs.read", path: "/etc/shadow" },
      ]),
    });

    const line = await handleLine(registry, request);
    const decoded = decodeLine(line) as JsonRpcErrorLine;
    expect(decoded.id).toBe(100);
    expect(decoded.error.code).toBe(ToolErrorCode.ToolCapabilityDenied);
    expect(decoded.error.message).toContain("/etc/shadow");
  });

  it("surfaces sync-deny through the dispatcher error envelope (-32003)", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const request = JSON.stringify({
      jsonrpc: JSON_RPC_VERSION,
      id: 101,
      method: "invokeTool",
      params: workerParams("echo", null, EMPTY_CAPS, [{ kind: "fs.write", path: "/etc/passwd" }]),
    });

    const line = await handleLine(registry, request);
    const decoded = decodeLine(line) as JsonRpcErrorLine;
    expect(decoded.id).toBe(101);
    expect(decoded.error.code).toBe(ToolErrorCode.ToolCapabilityDenied);
    expect(decoded.error.message).toContain("/etc/passwd");
  });
});

describe("invokeTool — wire boundary rejects spawnOptions (B1 regression)", () => {
  // CRITICAL: the dispatcher MUST NOT let a JSON-RPC payload pick the
  // worker bootstrap module. Wire-reachable bootstrapPath = arbitrary
  // code execution from the JSON-RPC boundary; see Phase 4 review B1.
  // These tests pin the structural defence: validateTool rejects ANY
  // wire payload carrying tool.spawnOptions with InvalidParams BEFORE
  // any spawn / runWorkerTool dispatch happens.
  it("rejects tool.spawnOptions on the typed in-process API with InvalidParams", async () => {
    const params: JsonRpcValue = {
      tool: {
        kind: "worker",
        method: "echo",
        capabilities: EMPTY_CAPS as unknown as JsonRpcValue,
        // The wire schema no longer carries this field — but a malicious
        // or buggy caller might send it anyway. Must fail closed.
        spawnOptions: {
          bootstrapPath: "/tmp/evil-bootstrap.mjs",
        } as unknown as JsonRpcValue,
      },
      input: null,
    };
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
      message: expect.stringContaining("spawnOptions") as unknown,
    });
  });

  it("rejects tool.spawnOptions through the JSON-RPC dispatcher (handleLine)", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const request = JSON.stringify({
      jsonrpc: JSON_RPC_VERSION,
      id: 200,
      method: "invokeTool",
      params: {
        tool: {
          kind: "worker",
          method: "echo",
          capabilities: EMPTY_CAPS,
          spawnOptions: { bootstrapPath: "/tmp/evil-bootstrap.mjs" },
        },
        input: null,
      },
    });

    const line = await handleLine(registry, request);
    const decoded = decodeLine(line) as JsonRpcErrorLine;
    expect(decoded.id).toBe(200);
    expect(decoded.error.code).toBe(JsonRpcErrorCode.InvalidParams);
    expect(decoded.error.message).toContain("spawnOptions");
  });

  it("rejects tool.spawnOptions even when the field is set to null", async () => {
    // Defence-in-depth: the `in` check fires on presence, not truthiness.
    // A `spawnOptions: null` payload still indicates the caller is
    // attempting to use a field the wire boundary forbids.
    const params: JsonRpcValue = {
      tool: {
        kind: "worker",
        method: "echo",
        capabilities: EMPTY_CAPS as unknown as JsonRpcValue,
        spawnOptions: null,
      },
      input: null,
    };
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });
});

describe("invokeTool — worker spawn failure / message branches", () => {
  it("translates a spawnWorker failure to ToolExecutionError", async () => {
    await expect(
      callWorker("echo", null, EMPTY_CAPS, undefined, {
        bootstrapPath: "/no/such/path/missing.mjs",
        readyTimeoutMs: 200,
      }),
    ).rejects.toMatchObject({
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
    const err = await callWorker("sigself", null, EMPTY_CAPS, undefined, {
      bootstrapPath: sigPath,
    }).catch((e: unknown) => e);
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
    await expect(
      callWorker("remoteCrash", null, EMPTY_CAPS, undefined, {
        bootstrapPath: remoteCrashPath,
      }),
    ).rejects.toMatchObject({
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
    const out = await callWorker("echo", { ok: 1 }, caps, [
      { kind: "net.connect", host: "example.com", port: 443 },
    ]);
    expect(out).toEqual({ ok: 1 });
  });

  it("accepts a port-less net.connect requiredOp", async () => {
    const caps: CapabilityDeclaration = {
      fs: { read: [], write: [] },
      net: { allow: ["raw.example.com"] },
      env: { allow: [] },
      proc: { spawn: false },
    };
    const out = await callWorker("echo", { ok: 1 }, caps, [
      { kind: "net.connect", host: "raw.example.com" },
    ]);
    expect(out).toEqual({ ok: 1 });
  });

  it("sync-denies a wire payload with no spawnOptions (proves wire path goes through worker branch)", async () => {
    // Wire shape never carries spawnOptions (B1). The deny short-circuits
    // before any spawn call would happen. This pins the `requiredOps`
    // worker branch via the JSON-RPC handler entry point.
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

  it("accepts a worker tool with no requiredOps (validates the requiredOps-undefined branch)", async () => {
    // Drives the validateTool branch where requiredOps is undefined.
    // The default bootstrap path under vitest resolves to a non-existent
    // dist/worker/bootstrap.js variant in some test setups; spawnWorker
    // rejects → ToolExecutionError. The wire callable accepts the
    // payload (no InvalidParams) — that's what the assertion proves.
    const params: JsonRpcValue = {
      tool: {
        kind: "worker",
        method: "ping",
        capabilities: EMPTY_CAPS as unknown as JsonRpcValue,
      },
      input: null,
    };
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
    const out = await callWorker("echo", { ok: 1 }, caps, [{ kind: "env.get", name: "HOME" }]);
    expect(out).toEqual({ ok: 1 });
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
