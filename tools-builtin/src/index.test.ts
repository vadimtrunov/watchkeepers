import { describe, expect, it } from "vitest";

import { BUILTIN_TOOLS } from "./index.js";

describe("BUILTIN_TOOLS", () => {
  it("is an empty frozen list until M9 lands", () => {
    expect(Array.isArray(BUILTIN_TOOLS)).toBe(true);
    expect(BUILTIN_TOOLS.length).toBe(0);
    expect(Object.isFrozen(BUILTIN_TOOLS)).toBe(true);
  });
});
