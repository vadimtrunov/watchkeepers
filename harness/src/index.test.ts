import { describe, expect, it } from "vitest";

import { banner } from "./index.js";

describe("banner", () => {
  it("returns the harness placeholder identifier", () => {
    const result = banner();
    expect(result.name).toBe("watchkeeper-harness");
    expect(result.placeholder).toBe(true);
  });
});
