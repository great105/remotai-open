import { describe, expect, it } from "vitest";
import { installedPairCode } from "./installedPair";
import { loginNextPath } from "./nextPath";

describe("installed app to account handoff", () => {
  it("preserves the code through a login URL and normalizes it for the form", () => {
    const next = "/infrastructure?add=1&type=computer&code=ab12cd34";
    expect(installedPairCode(loginNextPath(null, `?next=${encodeURIComponent(next)}`))).toBe("AB12-CD34");
  });
  it.each([
    "/infrastructure?code=AB12-CD34", "/start?add=1&code=AB12-CD34",
    "/infrastructure?add=1&code=", "/infrastructure?add=1&code=AB12-CD34extra",
    "/infrastructure?add=1&code=AB12-<x>", "https://other.test/infrastructure?add=1&code=AB12-CD34",
  ])("does not treat an unrelated or malformed URL as an installed-app handoff: %s", path => {
    expect(installedPairCode(path)).toBe("");
  });
});
