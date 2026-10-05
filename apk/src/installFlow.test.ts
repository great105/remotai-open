import { describe, expect, it } from "vitest";
import { installOS, pairInstructions } from "./installFlow";

describe("installation instructions follow the target, not the controller", () => {
  it("keeps a known OS from the URL and falls back for an invalid value", () => {
    expect(installOS("macos")).toBe("macos");
    expect(installOS("linux")).toBe("linux");
    expect(installOS("android")).toBe("windows");
    expect(installOS(null)).toBe("windows");
  });
  it.each(["macos", "linux"] as const)("uses the application for pairing and recovery on %s", os => {
    const step = pairInstructions(os, false, false);
    expect(step.text).toContain("В окне Remotai");
    expect(step.text).not.toContain("в терминале");
    expect(step.recovery).not.toContain("remotai pair");
    expect(step.recovery).toContain(os === "macos" ? "«Программы»" : "меню приложений");
  });
  it("gives server instructions even from a phone or a Windows browser", () => {
    expect(pairInstructions("windows", true, true)).toEqual(pairInstructions("linux", true, false));
  });
  it("does not pretend a phone can run the Windows installer", () => {
    expect(pairInstructions("windows", false, true).text).toContain("по переданной ссылке");
    expect(pairInstructions("windows", false, true).text).not.toContain("В окне Remotai");
  });
  it("retains the Windows window flow on the target computer", () => {
    expect(pairInstructions("windows", false, false).text).toContain("В окне Remotai");
  });
});
