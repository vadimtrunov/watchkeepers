/**
 * `spawnWorker` — host-side entry to the worker substrate.
 *
 * Forks `bootstrap.js` as a Node child with `serialization: 'advanced'`
 * (ADR §0001 mandates structured-clone semantics on the sub-channel),
 * sends the frozen capability declaration as the first IPC message,
 * and resolves on the child's `{kind:'ready'}` ack. The returned
 * {@link WorkerHandle} exposes a JSON-RPC request/notify/terminate API
 * plus a one-shot `'crash'` event — emitted with code -32004
 * ({@link ToolErrorCode.ToolWorkerCrashed}) when the child exits
 * before `terminate()` is called.
 */

// eslint-disable-next-line no-restricted-imports -- this module is the gated entry point that owns child_process.
import { fork, type ChildProcess } from "node:child_process";
import { fileURLToPath } from "node:url";

import type { CapabilityDeclaration } from "../capabilities.js";
import { ToolErrorCode } from "../invokeTool.js";
import type { JsonRpcValue } from "../types.js";

import type { WorkerInitMessage } from "./bootstrap.js";
import { IpcJsonRpcTransport, ipcChannelFromChildProcess } from "./transport.js";

export interface WorkerCrashEvent {
  readonly code: typeof ToolErrorCode.ToolWorkerCrashed;
  readonly exitCode?: number;
  readonly signal?: NodeJS.Signals;
}

export interface WorkerHandle {
  request(method: string, params?: JsonRpcValue): Promise<JsonRpcValue>;
  notify(method: string, params?: JsonRpcValue): void;
  /** Graceful shutdown; SIGKILL after a 1 s grace if the child still has not exited. */
  terminate(): Promise<void>;
  on(event: "crash", handler: (event: WorkerCrashEvent) => void): void;
  off(event: "crash", handler: (event: WorkerCrashEvent) => void): void;
}

export interface SpawnWorkerOptions {
  readonly bootstrapPath?: string;
  /** Init handshake budget (ms). Default 5000. */
  readonly readyTimeoutMs?: number;
  /** Test-only: ask the bootstrap to `process.exit(1)` after sending `ready`. */
  readonly crashOnInit?: boolean;
}

const DEFAULT_READY_TIMEOUT_MS = 5000;
const TERMINATE_GRACE_MS = 1000;

function defaultBootstrapPath(): string {
  return fileURLToPath(new URL("./bootstrap.js", import.meta.url));
}

export function spawnWorker(
  capabilities: CapabilityDeclaration,
  opts: SpawnWorkerOptions = {},
): Promise<WorkerHandle> {
  const bootstrapPath = opts.bootstrapPath ?? defaultBootstrapPath();
  const readyTimeoutMs = opts.readyTimeoutMs ?? DEFAULT_READY_TIMEOUT_MS;
  const child: ChildProcess = fork(bootstrapPath, [], {
    stdio: ["ignore", "pipe", "pipe", "ipc"],
    serialization: "advanced",
  });
  return waitForReady(child, capabilities, readyTimeoutMs, opts.crashOnInit === true);
}

function waitForReady(
  child: ChildProcess,
  capabilities: CapabilityDeclaration,
  readyTimeoutMs: number,
  crashOnInit: boolean,
): Promise<WorkerHandle> {
  return new Promise<WorkerHandle>((resolve, reject) => {
    let settled = false;

    // Single stateful exit handler shared between the pre-ready and post-ready
    // phases. Keeping one listener avoids the one-tick gap where a child exit
    // between settle() removing onEarlyExit and buildHandle attaching its own
    // listener would go unobserved (Comment 3 fix).
    let postReadyExitHandler:
      | ((code: number | null, signal: NodeJS.Signals | null) => void)
      | undefined;

    const onExit = (code: number | null, signal: NodeJS.Signals | null): void => {
      if (settled) {
        // Post-ready phase: delegate to the handler installed by buildHandle.
        postReadyExitHandler?.(code, signal);
      } else {
        // Pre-ready phase: treat as early exit.
        settle(() => {
          const detail =
            code !== null
              ? `exitCode=${String(code)}`
              : signal !== null
                ? `signal=${signal}`
                : "unknown cause";
          reject(new Error(`worker exited before ready: ${detail}`));
        });
      }
    };

    const settle = (fn: () => void): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      child.off("message", onInitMessage);
      child.off("error", onEarlyError);
      // Note: onExit is intentionally NOT removed here — it stays attached
      // so exits in the post-ready phase are observed via postReadyExitHandler.
      fn();
    };

    const timer = setTimeout(() => {
      settle(() => {
        try {
          child.kill("SIGKILL");
        } catch {
          /* already dead */
        }
        reject(new Error(`worker did not become ready within ${String(readyTimeoutMs)}ms`));
      });
    }, readyTimeoutMs);

    const onInitMessage = (message: unknown): void => {
      if (typeof message !== "object" || message === null) return;
      const env = message as { kind?: unknown; message?: unknown };
      if (env.kind === "ready") {
        settle(() => {
          resolve(
            buildHandle(child, capabilities, (handler) => {
              postReadyExitHandler = handler;
            }),
          );
        });
        return;
      }
      if (env.kind === "error") {
        const errMsg = typeof env.message === "string" ? env.message : "worker init failed";
        settle(() => {
          reject(new Error(errMsg));
        });
      }
    };

    const onEarlyError = (err: Error): void => {
      settle(() => {
        reject(err);
      });
    };

    child.on("message", onInitMessage);
    child.on("exit", onExit);
    child.on("error", onEarlyError);

    const initPayload: WorkerInitMessage = crashOnInit
      ? { kind: "init", capabilities, crashOnInit: true }
      : { kind: "init", capabilities };
    try {
      child.send(initPayload, (err) => {
        if (err) {
          settle(() => {
            reject(err);
          });
        }
      });
    } catch (err) {
      settle(() => {
        reject(err instanceof Error ? err : new Error(String(err)));
      });
    }
  });
}

function buildHandle(
  child: ChildProcess,
  _capabilities: CapabilityDeclaration,
  registerExitHandler: (
    handler: (code: number | null, signal: NodeJS.Signals | null) => void,
  ) => void,
): WorkerHandle {
  // `_capabilities` held for the M5.3.b.b.d gating wire-up.
  void _capabilities;

  const transport = new IpcJsonRpcTransport(ipcChannelFromChildProcess(child));
  const crashHandlers = new Set<(event: WorkerCrashEvent) => void>();
  let terminating = false;
  let exited = false;
  let crashEmitted = false;
  let exitResolve: (() => void) | undefined;
  const exitPromise = new Promise<void>((res) => {
    exitResolve = res;
  });

  // Register the post-ready exit handler via the callback provided by
  // waitForReady. This wires up BEFORE waitForReady's settle() removes the
  // pre-ready listener, eliminating the one-tick gap (Comment 3 fix).
  registerExitHandler((code, signal) => {
    exited = true;
    transport.dispose();
    exitResolve?.();
    if (!terminating && !crashEmitted) {
      crashEmitted = true;
      const event = buildCrashEvent(code, signal);
      for (const handler of [...crashHandlers]) handler(event);
    }
  });

  transport.onParseError(() => {
    /* M5.3.b.b.d wires structured logging here. */
  });

  return {
    request: (method, params) => transport.request(method, params),
    notify: (method, params) => {
      transport.notify(method, params);
    },
    terminate: async () => {
      if (exited) return;
      terminating = true;
      try {
        child.disconnect();
      } catch {
        /* already disconnected */
      }
      const killTimer = setTimeout(() => {
        if (!exited) {
          try {
            child.kill("SIGKILL");
          } catch {
            /* already dead */
          }
        }
      }, TERMINATE_GRACE_MS);
      try {
        await exitPromise;
      } finally {
        clearTimeout(killTimer);
      }
    },
    on: (_event, handler) => {
      crashHandlers.add(handler);
    },
    off: (_event, handler) => {
      crashHandlers.delete(handler);
    },
  };
}

/** Translate Node's `'exit'` payload into a {@link WorkerCrashEvent}. Exported for unit tests. */
export function buildCrashEvent(
  code: number | null,
  signal: NodeJS.Signals | null,
): WorkerCrashEvent {
  const base = { code: ToolErrorCode.ToolWorkerCrashed } as const;
  if (code !== null && signal !== null) return { ...base, exitCode: code, signal };
  if (code !== null) return { ...base, exitCode: code };
  if (signal !== null) return { ...base, signal };
  return base;
}
