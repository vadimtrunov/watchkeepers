/**
 * Worker child entry point. Lifecycle:
 *   1. Parent forks → first IPC message is `{kind:'init', capabilities, crashOnInit?}`.
 *   2. Bootstrap validates with `CapabilityDeclarationSchema.strict()`,
 *      `Object.freeze`s the result, and replies `{kind:'ready'}` (or
 *      `{kind:'error', message}` then exits 1 on validation failure).
 *   3. Bootstrap enters a JSON-RPC loop. Built-ins: `ping → 'pong'`,
 *      `log` notification (no response).
 *
 * `crashOnInit` is a test hook that exits(1) right after `ready`, so
 * the parent-side crash test does not need a separate fixture worker.
 */

import { fileURLToPath } from "node:url";

import { CapabilityDeclarationSchema } from "../capabilities.js";
import { JsonRpcErrorCode } from "../types.js";

import { IpcJsonRpcTransport, ipcChannelFromProcess } from "./transport.js";

export interface WorkerInitMessage {
  readonly kind: "init";
  readonly capabilities: unknown;
  readonly crashOnInit?: boolean;
}

export interface WorkerReadyMessage {
  readonly kind: "ready";
}

export interface WorkerInitErrorMessage {
  readonly kind: "error";
  readonly message: string;
}

export function runBootstrap(proc: NodeJS.Process): void {
  const initListener = (message: unknown): void => {
    if (!isInitMessage(message)) {
      proc.send?.({
        kind: "error",
        message: "first IPC message must be {kind:'init', capabilities}",
      } satisfies WorkerInitErrorMessage);
      proc.exit(1);
      return;
    }

    const parsed = CapabilityDeclarationSchema.strict().safeParse(message.capabilities);
    if (!parsed.success) {
      proc.send?.({
        kind: "error",
        message: `capability declaration invalid: ${parsed.error.message}`,
      } satisfies WorkerInitErrorMessage);
      proc.exit(1);
      return;
    }

    const capabilities = Object.freeze(parsed.data);
    proc.off("message", initListener);
    proc.send?.({ kind: "ready" } satisfies WorkerReadyMessage);

    if (message.crashOnInit === true) {
      proc.exit(1);
      return;
    }
    enterJsonRpcLoop(proc, capabilities);
  };

  proc.on("message", initListener);
}

function isInitMessage(message: unknown): message is WorkerInitMessage {
  if (typeof message !== "object" || message === null || Array.isArray(message)) return false;
  return (message as { kind?: unknown }).kind === "init";
}

function enterJsonRpcLoop(proc: NodeJS.Process, capabilities: Readonly<unknown>): void {
  // TODO(M5.3.b.b.e): runtime capability enforcement (intercept fs / net /
  // proc / env from inside the worker). The dispatcher's pre-gate via
  // `requiredOps` (M5.3.b.b.d) is currently the only check — a tool body
  // can still call e.g. `fs.readFileSync` directly with no gate.
  // `capabilities` is retained on this stack frame so the next executor
  // does not need to re-thread it through bootstrap when wiring runtime
  // interception.
  void capabilities;
  const transport = new IpcJsonRpcTransport(ipcChannelFromProcess(proc));
  transport.onRequest((req) => {
    if (req.method === "ping") {
      transport.sendResponse(req.id, "pong");
      return;
    }
    transport.sendError(req.id, JsonRpcErrorCode.MethodNotFound, `method not found: ${req.method}`);
  });
  transport.onNotification(() => {
    /* `log` and unknown notifications are observed-only per JSON-RPC §4.1. */
  });
}

// Direct-invocation guard: vitest workers also have `process.send`, so
// we additionally check that this module is the process entry point.
const isDirectInvocation =
  typeof process.argv[1] === "string" && fileURLToPath(import.meta.url) === process.argv[1];
if (isDirectInvocation && typeof process.send === "function") {
  runBootstrap(process);
}
