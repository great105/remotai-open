import { describe, expect, it } from "vitest";
import { isShortLandscape } from "./shortLandscape";

describe("short landscape terminal layout", () => {
  it("collapses the keys on a rotated phone", () => {
    expect(isShortLandscape(915, 412, 364)).toBe(true);
  });

  it("keeps the person's keys preference when a portrait keyboard shortens the viewport", () => {
    expect(isShortLandscape(412, 915, 300)).toBe(false);
  });
});
