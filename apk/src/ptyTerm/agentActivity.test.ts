import { describe, expect, it } from "vitest";
import { agentActivityBusy } from "./agentActivity";

describe("terminal activity agrees with the list's runtime status", () => {
  it("keeps a silent running or stalled turn working", () => {
    expect(agentActivityBusy("working", false)).toBe(true);
    expect(agentActivityBusy("stalled", false)).toBe(true);
  });
  it("does not interpret an idle repaint or typing a draft as a running turn", () => {
    for (const status of ["ready", "idle", "waiting", "error", "dead"]) {
      expect(agentActivityBusy(status, true)).toBe(false);
    }
  });
  it("uses fresh output only when the backend exposes no known status", () => {
    expect(agentActivityBusy(undefined, true)).toBe(true);
    expect(agentActivityBusy(undefined, false)).toBe(false);
  });
});
