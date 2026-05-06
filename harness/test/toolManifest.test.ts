/**
 * `toolManifest` schema and `deriveToolSchemas` tests — covers the
 * Tool Manifest wire shape and the per-tool zod-schema derivation
 * helper introduced in M5.3.c.a.
 *
 * The module is schema-only (no I/O), so tests assert on `safeParse`
 * outcomes, the zod issue paths surfaced on rejection, and the
 * behaviour of the synthesized validators returned by
 * {@link deriveToolSchemas}. Boot-time wiring (M5.3.c.b/c) and runtime
 * dispatch integration land in later slices and are exercised
 * separately.
 *
 * Mirrors the structure of `harness/test/capabilities.test.ts`.
 */

import { describe, expect, it } from "vitest";
import type { ZodTypeAny } from "zod";

import type { CapabilityDeclaration } from "../src/capabilities.js";
import {
  ToolManifestEntrySchema,
  ToolManifestError,
  ToolManifestSchema,
  deriveToolSchemas,
  type ToolManifest,
  type ToolManifestEntry,
} from "../src/toolManifest.js";

const NO_CAPS: CapabilityDeclaration = {
  fs: { read: [], write: [] },
  net: { allow: [] },
  env: { allow: [] },
  proc: { spawn: false },
};

/**
 * Test helper — assert the derived map carries `id` and return its
 * validator typed as `ZodTypeAny`. Avoids the non-null-assertion lint
 * rule by failing the test with a descriptive message when the entry
 * is unexpectedly missing.
 */
function getValidator(map: Map<string, ZodTypeAny>, id: string): ZodTypeAny {
  const v = map.get(id);
  if (v === undefined) throw new Error(`derived map missing entry for id=${id}`);
  return v;
}

describe("ToolManifestSchema — happy paths", () => {
  it("parses a 2-entry manifest with object-shaped inputs and per-entry capabilities", () => {
    const manifest: ToolManifest = {
      tools: [
        {
          id: "echo",
          description: "echoes a string back",
          input: {
            kind: "object",
            properties: { message: { kind: "string" } },
            required: ["message"],
          },
          capabilities: NO_CAPS,
        },
        {
          id: "sum",
          input: {
            kind: "object",
            properties: {
              a: { kind: "number" },
              b: { kind: "number" },
              tag: { kind: "string" },
            },
            required: ["a", "b"],
          },
          capabilities: {
            fs: { read: ["/tmp"], write: [] },
            net: { allow: [] },
            env: { allow: ["HOME"] },
            proc: { spawn: false },
          },
        },
      ],
    };

    const parsed = ToolManifestSchema.parse(manifest);
    expect(parsed.tools).toHaveLength(2);

    const derived = deriveToolSchemas(parsed);
    expect(derived.size).toBe(2);
    expect(derived.has("echo")).toBe(true);
    expect(derived.has("sum")).toBe(true);

    expect(getValidator(derived, "echo").parse({ message: "hi" })).toEqual({ message: "hi" });
    expect(getValidator(derived, "sum").parse({ a: 1, b: 2 })).toEqual({ a: 1, b: 2 });
    expect(getValidator(derived, "sum").parse({ a: 1, b: 2, tag: "x" })).toEqual({
      a: 1,
      b: 2,
      tag: "x",
    });
  });

  it("ToolManifestEntrySchema parses a single entry without a description", () => {
    const entry: ToolManifestEntry = {
      id: "noop",
      input: { kind: "null" },
      capabilities: NO_CAPS,
    };
    const parsed = ToolManifestEntrySchema.parse(entry);
    expect(parsed).toEqual(entry);
  });
});

describe("ToolInputSpec — primitive coverage", () => {
  it("derives a string-accepting schema for kind=string", () => {
    const derived = deriveToolSchemas(
      ToolManifestSchema.parse({
        tools: [{ id: "s", input: { kind: "string" }, capabilities: NO_CAPS }],
      }),
    );
    const validator = getValidator(derived, "s");
    expect(validator.parse("hello")).toBe("hello");
    expect(validator.safeParse(1).success).toBe(false);
  });

  it("derives a number-accepting schema for kind=number", () => {
    const derived = deriveToolSchemas(
      ToolManifestSchema.parse({
        tools: [{ id: "n", input: { kind: "number" }, capabilities: NO_CAPS }],
      }),
    );
    const validator = getValidator(derived, "n");
    expect(validator.parse(42)).toBe(42);
    expect(validator.safeParse("nope").success).toBe(false);
  });

  it("derives a boolean-accepting schema for kind=boolean", () => {
    const derived = deriveToolSchemas(
      ToolManifestSchema.parse({
        tools: [{ id: "b", input: { kind: "boolean" }, capabilities: NO_CAPS }],
      }),
    );
    const validator = getValidator(derived, "b");
    expect(validator.parse(true)).toBe(true);
    expect(validator.safeParse(0).success).toBe(false);
  });

  it("derives an array-accepting schema for kind=array", () => {
    const derived = deriveToolSchemas(
      ToolManifestSchema.parse({
        tools: [
          {
            id: "a",
            input: { kind: "array", items: { kind: "number" } },
            capabilities: NO_CAPS,
          },
        ],
      }),
    );
    const validator = getValidator(derived, "a");
    expect(validator.parse([1, 2, 3])).toEqual([1, 2, 3]);
    expect(validator.safeParse([1, "x"]).success).toBe(false);
  });

  it("derives a null-accepting schema for kind=null", () => {
    const derived = deriveToolSchemas(
      ToolManifestSchema.parse({
        tools: [{ id: "z", input: { kind: "null" }, capabilities: NO_CAPS }],
      }),
    );
    const validator = getValidator(derived, "z");
    expect(validator.parse(null)).toBeNull();
    expect(validator.safeParse(undefined).success).toBe(false);
  });

  it("treats keys absent from required as optional on derived object schemas", () => {
    const derived = deriveToolSchemas(
      ToolManifestSchema.parse({
        tools: [
          {
            id: "opt",
            input: {
              kind: "object",
              properties: { a: { kind: "string" }, b: { kind: "string" } },
              required: ["a"],
            },
            capabilities: NO_CAPS,
          },
        ],
      }),
    );
    const validator = getValidator(derived, "opt");
    expect(validator.parse({ a: "x" })).toEqual({ a: "x" });
    expect(validator.parse({ a: "x", b: "y" })).toEqual({ a: "x", b: "y" });
    // optional key supplied with wrong type must be rejected
    expect(validator.safeParse({ a: "x", b: 42 }).success).toBe(false);
    // unknown keys must be rejected (strict posture)
    expect(validator.safeParse({ a: "x", extra: "sneak" }).success).toBe(false);
  });
});

describe("ToolManifestSchema — negative cases", () => {
  it("rejects an unknown top-level field via .strict()", () => {
    const result = ToolManifestSchema.safeParse({
      tools: [],
      extra: "nope",
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const codes = result.error.issues.map((i) => i.code);
      expect(codes).toContain("unrecognized_keys");
    }
  });

  it("rejects an entry with empty id (.min(1))", () => {
    const result = ToolManifestSchema.safeParse({
      tools: [
        {
          id: "",
          input: { kind: "string" },
          capabilities: NO_CAPS,
        },
      ],
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const issue = result.error.issues.find(
        (i) => i.path[0] === "tools" && i.path[1] === 0 && i.path[2] === "id",
      );
      expect(issue).toBeDefined();
      expect(issue?.code).toBe("too_small");
    }
  });

  it("rejects an entry whose input.kind is unknown via the discriminated union", () => {
    const result = ToolManifestSchema.safeParse({
      tools: [
        {
          id: "bogus",
          input: { kind: "date" },
          capabilities: NO_CAPS,
        },
      ],
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const codes = result.error.issues.map((i) => i.code);
      expect(codes).toContain("invalid_union_discriminator");
    }
  });

  it("rejects an entry missing the required capabilities field", () => {
    const result = ToolManifestSchema.safeParse({
      tools: [
        {
          id: "t",
          input: { kind: "string" },
        },
      ],
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const issue = result.error.issues.find(
        (i) => i.path[0] === "tools" && i.path[1] === 0 && i.path[2] === "capabilities",
      );
      expect(issue).toBeDefined();
      expect(issue?.code).toBe("invalid_type");
    }
  });

  it("rejects an entry with extra top-level field via .strict()", () => {
    const result = ToolManifestSchema.safeParse({
      tools: [
        {
          id: "t",
          input: { kind: "string" },
          capabilities: NO_CAPS,
          extra: "nope",
        },
      ],
    });
    expect(result.success).toBe(false);
    if (!result.success) {
      const codes = result.error.issues.map((i) => i.code);
      expect(codes).toContain("unrecognized_keys");
    }
  });
});

describe("deriveToolSchemas — collision policy", () => {
  it("throws ToolManifestError referencing the conflicting id on duplicate ids", () => {
    const manifest: ToolManifest = {
      tools: [
        { id: "dup", input: { kind: "string" }, capabilities: NO_CAPS },
        { id: "dup", input: { kind: "number" }, capabilities: NO_CAPS },
      ],
    };
    expect(() => deriveToolSchemas(manifest)).toThrow(ToolManifestError);
    expect(() => deriveToolSchemas(manifest)).toThrow(/dup/);
  });
});

describe("deriveToolSchemas — derived input enforcement", () => {
  it("rejects an input that violates the entry's declared input spec", () => {
    const manifest = ToolManifestSchema.parse({
      tools: [
        {
          id: "echo",
          input: {
            kind: "object",
            properties: { message: { kind: "string" } },
            required: ["message"],
          },
          capabilities: NO_CAPS,
        },
      ],
    });
    const derived = deriveToolSchemas(manifest);
    const validator = getValidator(derived, "echo");
    // missing required key
    expect(validator.safeParse({}).success).toBe(false);
    // wrong type for the declared key
    expect(validator.safeParse({ message: 42 }).success).toBe(false);
  });
});

describe("ToolManifestError", () => {
  it("carries name=ToolManifestError so callers can pattern-match", () => {
    const err = new ToolManifestError("boom");
    expect(err).toBeInstanceOf(Error);
    expect(err.name).toBe("ToolManifestError");
    expect(err.message).toBe("boom");
  });
});
