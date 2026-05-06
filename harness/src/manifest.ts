/**
 * Active-toolset state for the harness ACL gate (M5.5.b.a).
 *
 * The Go core projects `manifest_version.tools` jsonb into a
 * `runtime.Manifest.Toolset []string` of tool names (see
 * `core/pkg/manifest/loader.go`). At session boot the orchestrator
 * delivers that list over the JSON-RPC wire via the `setManifest`
 * method; this module holds the single source of truth the
 * `invokeTool` handler consults BEFORE dispatching to
 * {@link runIsolatedJs} / {@link runWorkerTool}.
 *
 * Posture is deny-by-default: until `setManifest` is called the active
 * toolset is `undefined`, which the ACL gate treats as an empty allow-
 * list — every invocation surfaces a `ToolUnauthorized` JSON-RPC
 * error. After `setManifest({ toolset: [] })` the state is the empty
 * array (still deny-all but explicit, matches `runtime.go:99-103`).
 *
 * Future scopes (M5.5.b.b, M5.5.b.c) widen this module with additional
 * manifest projections (model, autonomy, authority matrix). The
 * toolset slice is intentionally first-in because it is the one the
 * harness boundary needs to enforce immediately.
 */

/**
 * Active toolset, or `undefined` when `setManifest` has not yet been
 * called. The ACL gate distinguishes "never set" from "set to empty"
 * only in observability — both paths reject every tool invocation.
 */
let activeToolset: readonly string[] | undefined;

/**
 * Return the currently active toolset, or `undefined` when no
 * `setManifest` call has yet been honoured. Callers MUST treat
 * `undefined` and the empty array as the same deny-all decision; the
 * distinction exists only for telemetry / "did the orchestrator ever
 * deliver a manifest?" introspection.
 */
export function getActiveToolset(): readonly string[] | undefined {
  return activeToolset;
}

/**
 * Replace the active toolset with `names`. Stores a defensive copy so
 * subsequent caller-side mutation cannot retroactively widen the
 * allow-list. Caller is responsible for shape validation; the
 * `setManifest` JSON-RPC handler does that via zod before reaching
 * this entry point.
 */
export function setActiveToolset(names: readonly string[]): void {
  activeToolset = [...names];
}

/**
 * Test-only reset. Clears the module-level state so each test starts
 * from the deny-by-default posture without leaking across files. NOT
 * exposed on the wire and NOT called from production code paths — the
 * orchestrator's lifecycle (one harness process per session) means a
 * runtime reset is unnecessary.
 */
export function __resetActiveToolsetForTests(): void {
  activeToolset = undefined;
}
