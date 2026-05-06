/**
 * Capability declaration and gating policy schemas for the
 * worker-process tool path (ADR `docs/adr/0001-worker-substrate.md`).
 *
 * The worker substrate enforces I/O on four axes — filesystem, network
 * egress, environment variables, and child-process spawn — and the
 * declaration is **frozen at worker spawn time** (immutable for the
 * worker's lifetime). This module ships the wire-shape contract only;
 * dispatcher wiring lands in M5.3.b.b.d and worker spawn in M5.3.b.b.c.
 *
 * Both schemas are `.strict()` so a typo or future-protocol field on the
 * client side surfaces as a validation error rather than silently being
 * dropped — matches the conservative posture the ADR prescribes for the
 * gating layer.
 */

import { z } from "zod";

/**
 * JSON-RPC error code reserved by ADR §0001 for "the worker rejected the
 * call because the caller's frozen capability set forbids the requested
 * I/O". Lives at the top of the application slice (-32099..-32000)
 * alongside {@link import("./invokeTool.js").ToolErrorCode}; re-exported
 * here as a constant so capability-aware callers can reference it
 * without importing the full enum.
 */
export const WORKER_CAPABILITY_ERROR_CODE = -32003 as const;

/**
 * Worker capability declaration.
 *
 * Shape:
 *   - `fs.read`   — absolute paths the worker may read from
 *   - `fs.write`  — absolute paths the worker may write to
 *   - `net.allow` — network egress allowlist (host[:port])
 *   - `env.allow` — process env vars exposed to the worker
 *   - `proc.spawn`— may the worker spawn child processes?
 *
 * All sub-objects and lists are required; lists may be empty (an empty
 * list means "deny all on this axis"). `.strict()` rejects any
 * additional top-level / sub-object keys so the wire contract stays
 * versioned through explicit schema changes.
 */
export const CapabilityDeclarationSchema = z
  .object({
    fs: z
      .object({
        read: z.array(z.string()),
        write: z.array(z.string()),
      })
      .strict(),
    net: z
      .object({
        allow: z.array(z.string()),
      })
      .strict(),
    env: z
      .object({
        allow: z.array(z.string()),
      })
      .strict(),
    proc: z
      .object({
        spawn: z.boolean(),
      })
      .strict(),
  })
  .strict();

/**
 * Inferred TypeScript type for a parsed {@link CapabilityDeclarationSchema}.
 * Re-exported so callers can pass typed declarations into the worker
 * spawn path without re-deriving via `z.infer` at every site.
 */
export type CapabilityDeclaration = z.infer<typeof CapabilityDeclarationSchema>;

/**
 * Outcome of a single capability gate check.
 *
 * `allow: true`  — the requested I/O is within the frozen declaration.
 * `allow: false` — the gate denies; `reason` SHOULD be populated for
 *                  observability but is not required (the wire contract
 *                  treats it as advisory, not load-bearing).
 *
 * `.strict()` mirrors the declaration schema — extra fields signal a
 * version skew the harness should not silently absorb.
 */
export const GatingPolicyDecisionSchema = z
  .object({
    allow: z.boolean(),
    reason: z.string().optional(),
  })
  .strict();

/**
 * Inferred TypeScript type for a parsed {@link GatingPolicyDecisionSchema}.
 */
export type GatingPolicyDecision = z.infer<typeof GatingPolicyDecisionSchema>;
