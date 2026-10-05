import { describe, expect, it } from "vitest";
import { isTerminalHermesSubagentStatus, normalizeHermesSubagents } from "./subagents";

describe("the confirmed Hermes subagent roster", () => {
  it("distinguishes active, terminal and unknown native statuses without reading model prose", () => {
    const statuses = ["queued", "running", "completed", "failed", "error", "timeout", "interrupted", "future-status"];
    const rows = normalizeHermesSubagents({ subagents: statuses.map((status, index) => ({
      subagent_id: `child-${index}`, goal: "I am still working", status, tool_count: index, last_tool: "read_file",
    })) });
    expect(rows.map(row => row.status)).toEqual(["running", "running", "completed", "failed", "failed", "failed", "cancelled", "unknown"]);
    expect(rows.map(row => isTerminalHermesSubagentStatus(row.status))).toEqual([false, false, true, true, true, true, true, false]);
    expect(rows[0]).toMatchObject({ id: "child-0", rawStatus: "queued", toolCount: 0, lastTool: "read_file" });
    expect(rows[7].goal).toBe("I am still working");
  });

  it("only accepts an explicit successful empty roster as evidence that no children were returned", () => {
    expect(normalizeHermesSubagents({ subagents: [], delegations: [] })).toEqual([]);
    for (const value of [null, undefined, [], {}, { error: "unsupported" }, { subagents: null }, { subagents: {} }]) {
      expect(() => normalizeHermesSubagents(value)).toThrow("Hermes не подтвердил список помощников.");
    }
  });

  it("retains a missing or future status as unknown rather than silently completing the child", () => {
    const rows = normalizeHermesSubagents({ subagents: [
      { subagent_id: "without-status", goal: "Task" },
      { subagent_id: "unknown-status", status: "done", goal: "Task" },
    ] });
    expect(rows.map(row => row.status)).toEqual(["unknown", "unknown"]);
    expect(rows.every(row => !isTerminalHermesSubagentStatus(row.status))).toBe(true);
  });

  it("rejects broken or duplicate identities rather than producing a false empty or double-counted roster", () => {
    for (const subagents of [[null], [{}], [{ subagent_id: " " }], [{ subagent_id: 42 }],
      [{ subagent_id: "same" }, { subagent_id: "same" }]]) {
      expect(() => normalizeHermesSubagents({ subagents })).toThrow();
    }
  });

  it("never exposes arbitrary metadata as a goal, tool label or count", () => {
    const row = normalizeHermesSubagents({ subagents: [{
      subagent_id: "child", status: "running", goal: { signature: "opaque" },
      tool_count: -1, last_tool: { private: "opaque" }, extra: "opaque",
    }] })[0];
    expect(row).toEqual({ id: "child", goal: "", status: "running", toolCount: 0, rawStatus: "running" });
  });
});
