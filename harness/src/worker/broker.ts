/**
 * Capability-gating helpers for the worker tool path.
 *
 * `gateToolInvocation` makes a synchronous allow/deny decision for a
 * single requested I/O operation against the frozen
 * {@link CapabilityDeclaration}. The dispatcher calls this BEFORE
 * `spawnWorker` so denials never pay the fork cost (ADR §0001 — "easier
 * to enforce at a process boundary, but cheap statically-deniable
 * checks belong on the host").
 *
 * The gate is intentionally minimal: each {@link ToolOperation} has one
 * matching axis on the declaration, and the check is a literal
 * allowlist match (path / host[:port] / env-name) plus the boolean
 * `proc.spawn` toggle. Future axes (signals, IPC) plug in by extending
 * the discriminated union.
 */

import type { CapabilityDeclaration, GatingPolicyDecision } from "../capabilities.js";

/**
 * Tagged union of statically-deniable tool I/O operations. The
 * dispatcher passes one or more of these via `WorkerTool.requiredOps`
 * so the broker can short-circuit a spawn that would unconditionally be
 * rejected at the worker boundary.
 */
export type ToolOperation =
  | { readonly kind: "fs.read"; readonly path: string }
  | { readonly kind: "fs.write"; readonly path: string }
  | { readonly kind: "net.connect"; readonly host: string; readonly port?: number }
  | { readonly kind: "env.get"; readonly name: string }
  | { readonly kind: "proc.spawn" };

/**
 * Allow-or-deny a single operation against `declaration`. Pure; never
 * throws. The returned `reason` (on deny) is advisory, populated for
 * observability — the dispatcher surfaces it in the JSON-RPC error
 * `data` field so callers can debug their capability declaration.
 */
export function gateToolInvocation(
  declaration: CapabilityDeclaration,
  op: ToolOperation,
): GatingPolicyDecision {
  switch (op.kind) {
    case "fs.read":
      return declaration.fs.read.includes(op.path)
        ? { allow: true }
        : {
            allow: false,
            reason: `fs.read denied: path "${op.path}" not in fs.read allowlist`,
          };
    case "fs.write":
      return declaration.fs.write.includes(op.path)
        ? { allow: true }
        : {
            allow: false,
            reason: `fs.write denied: path "${op.path}" not in fs.write allowlist`,
          };
    case "net.connect": {
      const target = op.port === undefined ? op.host : `${op.host}:${String(op.port)}`;
      // Accept either a bare host match or a `host:port` match — callers
      // may pin a port or leave it open in the declaration.
      const allowed =
        declaration.net.allow.includes(target) ||
        (op.port !== undefined && declaration.net.allow.includes(op.host));
      return allowed
        ? { allow: true }
        : {
            allow: false,
            reason: `net.connect denied: target "${target}" not in net.allow allowlist`,
          };
    }
    case "env.get":
      return declaration.env.allow.includes(op.name)
        ? { allow: true }
        : {
            allow: false,
            reason: `env.get denied: variable "${op.name}" not in env.allow allowlist`,
          };
    case "proc.spawn":
      return declaration.proc.spawn
        ? { allow: true }
        : { allow: false, reason: "proc.spawn denied: declaration disallows child processes" };
  }
}
