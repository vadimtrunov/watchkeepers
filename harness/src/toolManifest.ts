/**
 * Tool Manifest schema and `deriveToolSchemas` helper for the harness
 * boot-time tool registry (M5.3.c.a, ROADMAP §M5.3.c).
 *
 * The Tool Manifest is a declarative inventory of tools the harness
 * exposes to the planner / dispatcher: each entry pairs a stable `id`
 * with a JSON-Schema-shaped `input` contract and the existing
 * {@link CapabilityDeclaration} from `harness/src/capabilities.ts`.
 *
 * This module is **schema-only** — it ships the wire-shape contract and
 * a pure-function helper that turns a parsed manifest into a
 * `Map<toolId, ZodTypeAny>`. Boot-time integration (load Manifest at
 * harness startup) and dispatch wiring (consult derived schemas before
 * invoking a tool) arrive in M5.3.c.b/c.
 *
 * Both schemas are `.strict()` so a typo or future-protocol field on the
 * client side surfaces as a validation error rather than silently being
 * dropped — matches the conservative posture of M5.3.b.b.b
 * (capabilities) and the gating layer prescribed by ADR §0001.
 */

import { z, type ZodTypeAny } from "zod";

import { CapabilityDeclarationSchema } from "./capabilities.js";

/**
 * Sentinel error class thrown by {@link deriveToolSchemas} when the
 * manifest violates a derivation invariant the static schema cannot
 * express (currently: duplicate `id` across two entries).
 *
 * Lives next to the schema rather than as a plain `Error` so callers
 * (M5.3.c.b dispatch wiring, M5.5 boot loader) can pattern-match on
 * `instanceof ToolManifestError` to surface a structured wire response
 * instead of a generic 500.
 */
export class ToolManifestError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "ToolManifestError";
  }
}

/**
 * Discriminated zod union describing the input contract of a single
 * tool. The `kind` discriminator selects the underlying JSON-Schema-ish
 * primitive, matching the v1 subset agreed at Gate 2:
 *
 *   - `object`  — the canonical tool shape: `properties` (string→spec)
 *                 plus a `required` key list. Drives a synthesized
 *                 `z.object({...})` at derivation time, where every key
 *                 listed in `required` becomes a non-optional field and
 *                 the remaining keys become `.optional()`.
 *   - `string`  — derives `z.string()`.
 *   - `number`  — derives `z.number()`.
 *   - `boolean` — derives `z.boolean()`.
 *   - `array`   — derives `z.array(<items>)`, where `items` is itself a
 *                 ToolInputSpec so nested shapes compose recursively.
 *   - `null`    — derives `z.null()` (rare but useful for tools whose
 *                 input is intentionally empty / sentinel-only).
 *
 * Every nested object carries `.strict()` so an unknown field on the
 * wire surfaces as a validation error rather than being silently
 * dropped. This mirrors {@link CapabilityDeclarationSchema}'s posture.
 *
 * The recursive shape is expressed via a `z.lazy(() => …)` reference so
 * the type-level alias and the runtime schema agree on the recursion
 * point.
 */
export type ToolInputSpec =
  | {
      readonly kind: "object";
      readonly properties: Record<string, ToolInputSpec>;
      readonly required: readonly string[];
    }
  | { readonly kind: "string" }
  | { readonly kind: "number" }
  | { readonly kind: "boolean" }
  | { readonly kind: "array"; readonly items: ToolInputSpec }
  | { readonly kind: "null" };

export const ToolInputSpecSchema: z.ZodType<ToolInputSpec> = z.lazy(() =>
  z.discriminatedUnion("kind", [
    z
      .object({
        kind: z.literal("object"),
        properties: z.record(ToolInputSpecSchema),
        required: z.array(z.string()),
      })
      .strict(),
    z.object({ kind: z.literal("string") }).strict(),
    z.object({ kind: z.literal("number") }).strict(),
    z.object({ kind: z.literal("boolean") }).strict(),
    z
      .object({
        kind: z.literal("array"),
        items: ToolInputSpecSchema,
      })
      .strict(),
    z.object({ kind: z.literal("null") }).strict(),
  ]),
);

/**
 * Wire shape of a single Tool Manifest entry.
 *
 * `id` is the stable lookup key; downstream callers (M5.3.c.b dispatch
 * wiring) use it to fetch the derived schema from the
 * {@link deriveToolSchemas} map. `description` is optional human-facing
 * text for planner display. `input` is the JSON-Schema-ish input
 * contract, validated against {@link ToolInputSpecSchema}.
 * `capabilities` is the frozen {@link CapabilityDeclaration} the
 * worker-process tool path will receive at spawn (ADR §0001).
 *
 * `.strict()` rejects any additional top-level keys so the wire
 * contract stays versioned through explicit schema changes.
 */
export const ToolManifestEntrySchema = z
  .object({
    id: z.string().min(1),
    description: z.string().optional(),
    input: ToolInputSpecSchema,
    capabilities: CapabilityDeclarationSchema,
  })
  .strict();

/**
 * Inferred TypeScript type for a parsed
 * {@link ToolManifestEntrySchema}. Re-exported so callers can pass
 * typed entries between modules without re-deriving via `z.infer` at
 * every site.
 */
export type ToolManifestEntry = z.infer<typeof ToolManifestEntrySchema>;

/**
 * Top-level Tool Manifest shape: `{ tools: ToolManifestEntry[] }`.
 *
 * `.strict()` mirrors the entry schema — extra top-level fields signal
 * a version skew the harness should not silently absorb.
 */
export const ToolManifestSchema = z
  .object({
    tools: z.array(ToolManifestEntrySchema),
  })
  .strict();

/**
 * Inferred TypeScript type for a parsed {@link ToolManifestSchema}.
 */
export type ToolManifest = z.infer<typeof ToolManifestSchema>;

/**
 * Derive a per-tool input validator map from a parsed Tool Manifest.
 *
 * Walks `manifest.tools` and synthesizes a {@link ZodTypeAny} for each
 * entry's declared `input` spec, keyed by entry `id`. The returned map
 * is the contract M5.3.c.b dispatch wiring will consult before
 * invoking a tool — an unknown `id` means "no manifest entry" and an
 * input that fails its derived schema is rejected at the wire boundary
 * rather than reaching the tool.
 *
 * Collision policy is explicit (AC5): a duplicate `id` across two
 * entries throws {@link ToolManifestError} referencing the conflicting
 * id. Silent overwrite would let the second entry's schema replace the
 * first — a foot-gun the manifest contract refuses to absorb.
 */
export function deriveToolSchemas(manifest: ToolManifest): Map<string, ZodTypeAny> {
  const out = new Map<string, ZodTypeAny>();
  for (const entry of manifest.tools) {
    if (out.has(entry.id)) {
      throw new ToolManifestError(`duplicate tool id in manifest: ${entry.id}`);
    }
    out.set(entry.id, buildSchemaForSpec(entry.input));
  }
  return out;
}

/**
 * Recursively synthesize a zod schema from a single
 * {@link ToolInputSpec}. The `object` branch is the load-bearing case:
 * it walks the declared `properties`, marking every key NOT listed in
 * `required` as `.optional()`. The resulting `z.object({...})` is left
 * non-strict so callers can extend the wire shape additively without
 * refactoring every Manifest entry — strictness is enforced at the
 * Manifest schema layer, not the per-tool input layer.
 */
function buildSchemaForSpec(spec: ToolInputSpec): ZodTypeAny {
  switch (spec.kind) {
    case "object": {
      const required = new Set(spec.required);
      const shape: Record<string, ZodTypeAny> = {};
      for (const [key, child] of Object.entries(spec.properties)) {
        const childSchema = buildSchemaForSpec(child);
        shape[key] = required.has(key) ? childSchema : childSchema.optional();
      }
      return z.object(shape);
    }
    case "string":
      return z.string();
    case "number":
      return z.number();
    case "boolean":
      return z.boolean();
    case "array":
      return z.array(buildSchemaForSpec(spec.items));
    case "null":
      return z.null();
  }
}
