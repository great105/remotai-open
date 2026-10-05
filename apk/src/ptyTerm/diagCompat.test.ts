import { describe, expect, it } from "vitest";
import { LEGACY_DIAG_KINDS, diagReachesAgent, stateShowsNewAgent } from "./diagCompat";

// Все виды, которые шлёт клиент (grep sendDiag в PtyTermView), — новым и старым.
const NEW_KINDS = ["flow", "nav", "nav-shadow", "recovery", "trace-mark", "snapshot-grid-state",
  "alt-scroll-late-answer", "retention-shadow"];

describe("diag и старый агент (волна 4, I-14)", () => {
  it("старому агенту уходят только виды, которые он знал; новому — все", () => {
    for (const what of LEGACY_DIAG_KINDS) {
      expect(diagReachesAgent(what, false), what).toBe(true);
      expect(diagReachesAgent(what, true), what).toBe(true);
    }
    for (const what of NEW_KINDS) {
      expect(diagReachesAgent(what, false), what).toBe(false);
      expect(diagReachesAgent(what, true), what).toBe(true);
    }
    expect(diagReachesAgent("unknown-future", false)).toBe(false);
  });

  it("новый агент виден по полям /state, которых у 2.71.1 нет", () => {
    expect(stateShowsNewAgent({ fg_pid: 42, agent_kind: "codex" })).toBe(false); // 2.71.1
    expect(stateShowsNewAgent({ fg_pid: 42, fg_started: 1757000000123 })).toBe(true);
    expect(stateShowsNewAgent({ history_retention: "preserve" })).toBe(true);
    expect(stateShowsNewAgent({ history_retention: "wipe" })).toBe(false);
    expect(stateShowsNewAgent({ fg_started: "1" })).toBe(false);
    expect(stateShowsNewAgent(null)).toBe(false);
  });
});
