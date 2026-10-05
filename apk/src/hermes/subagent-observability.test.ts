import { describe, expect, it } from "vitest";
import { normalizeHermesSubagents } from "./subagents";
import { applyHermesEvent } from "./state";
import { emptyRunState } from "./client";

describe("safe subagent observation contracts", () => {
  it("rejects literal child IDs with surrounding whitespace instead of silently repairing them",()=>{
    expect(()=>normalizeHermesSubagents({subagents:[{subagent_id:" child "}]})).toThrow();
  });
  it("preserves only finite native child-start instants and safe tool identifiers", () => {
    const rows = normalizeHermesSubagents({ subagents: [
      { subagent_id: "a", started_at: 1710000000.25, last_tool: "read_file" },
      { subagent_id: "b", started_at: Infinity, last_tool: "terminal(password=secret)" },
    ] });
    expect(rows[0]).toMatchObject({ startedAt: 1710000000.25, lastTool: "read_file" });
    expect(rows[1]).not.toHaveProperty("startedAt");
    expect(rows[1]).not.toHaveProperty("lastTool");
  });
  it("never projects child thoughts or private preview into the shared activity lane", () => {
    const e = (seq: number, type: string) => ({ seq, frame: { method: "event", params: { session_id: "live", type,
      payload: { subagent_id: "child", goal: "Owned task", text: "PRIVATE_THOUGHT", tool_preview: "PRIVATE_ARGS", tool_name: "read_file" } } } });
    const initial = emptyRunState();
    expect(applyHermesEvent(initial, e(1, "subagent.thinking"), "live")).toBe(initial);
    const state = applyHermesEvent(initial, e(2, "subagent.tool"), "live");
    expect(JSON.stringify(state.activities)).not.toContain("PRIVATE");
  });
});
