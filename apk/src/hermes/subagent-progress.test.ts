import { describe, expect, it } from "vitest";
import { compactSubagentObservation, emptySubagentProgress, observeSubagentEvent, observeSubagentRoster } from "./subagent-progress";
const event = (seq: number, type = "subagent.tool", payload: Record<string, unknown> = {}) => ({ seq, frame: { method: "event", params: { session_id: "live", type, payload: { subagent_id: "child", tool_name: "read_file", ...payload } } } });
describe("bounded truthful child journal", () => {
  const roster = (status: "running" | "completed" | "failed" | "cancelled" | "unknown", id = "child") => [{ id, goal: "Task", status, toolCount: 1 }];
  const started = () => observeSubagentEvent(observeSubagentRoster(emptySubagentProgress("scope", "live"), roster("running"), 1), event(1), 2);
  it.each(["completed", "failed", "cancelled"] as const)("retains journal but stops compact selection after roster %s", status => {
    const state = observeSubagentRoster(started(), roster(status), 3);
    expect(compactSubagentObservation(state.children, roster(status))).toBeUndefined();
    expect(state.children[0].terminalConfirmed).toBeUndefined();
    expect(state.children[0].journal).toHaveLength(1);
  });
  it.each([{ rows: null }, { rows: roster("unknown") }])("does not select stale activity with unknown roster %j", ({ rows }) => {
    expect(compactSubagentObservation(started().children, rows)).toBeUndefined();
  });
  it("selects the current running helper rather than a newer terminal child", () => {
    const state = started();
    const running = { ...state.children[0], id: "running", receivedAt: 1 };
    const ended = { ...state.children[0], status: "completed" as const, receivedAt: 5 };
    expect(compactSubagentObservation([running, ended], [...roster("running", "running"), ...roster("completed")])?.id).toBe("running");
  });
  it("retains an absent live child as explicitly unconfirmed without changing its journal", () => {
    const state = observeSubagentRoster(started(), [], 3);
    expect(compactSubagentObservation(state.children, [])).toMatchObject({ present: false, status: "running" });
    expect(state.children[0].terminalConfirmed).toBeUndefined();
    expect(state.children[0].journal).toHaveLength(1);
  });
  it("keeps roster check separate from actual event receipt and never fabricates past tool rows", () => {
    let state = observeSubagentRoster(emptySubagentProgress("device/default/chat/epoch", "live"), [{ id: "child", goal: "Read tests", status: "running", lastTool: "terminal", toolCount: 9, startedAt: 100 }], 2000);
    expect(state.children[0]).toMatchObject({ checkedAt: 2000, journal: [], partial: true, startedAt: 100 });
    expect(state.children[0].receivedAt).toBeUndefined();
    state = observeSubagentEvent(state, event(1), 2100);
    expect(state.children[0]).toMatchObject({ lastTool: "read_file", receivedAt: 2100, journal: [{ id: "event:1", kind: "tool_start", receivedAt: 2100 }] });
    state = observeSubagentRoster(state, [{ id: "child", goal: "Read tests", status: "running", lastTool: "read_file", toolCount: 10 }], 3000);
    expect(state.children[0].receivedAt).toBe(2100);
    expect(state.children[0].startedAt).toBe(100);
    expect(state.children[0].checkedAt).toBe(3000);
  });
  it("deduplicates replay, drops thoughts and arguments, bounds 200 starts", () => {
    let state = observeSubagentRoster(emptySubagentProgress("scope", "live"), [{ id: "child", goal: "Task", status: "running", toolCount: 0 }], 1);
    state = observeSubagentEvent(state, event(1, "subagent.thinking", { text: "PRIVATE" }), 2);
    expect(state.children[0].journal).toEqual([]);
    for (let seq = 2; seq <= 205; seq++) state = observeSubagentEvent(state, event(seq, "subagent.tool", { tool_name: seq === 205 ? "terminal(secret)" : "read_file", text: "PRIVATE", tool_preview: "PRIVATE" }), seq);
    expect(observeSubagentEvent(state, event(205), 999)).toBe(state);
    expect(state.children[0].journal).toHaveLength(200);
    expect(state.children[0].journal[199].toolName).toBeUndefined();
    expect(JSON.stringify(state)).not.toContain("PRIVATE");
    expect(JSON.stringify(state)).not.toContain("secret");
    const foreign = event(206); foreign.frame.params.session_id = "other";
    expect(observeSubagentEvent(state, foreign, 999)).toBe(state);
  });
  it("does not double-count starts already included in a roster or repair malformed IDs", () => {
    let state = observeSubagentRoster(emptySubagentProgress("scope","live"),[{id:"child",goal:"Task",status:"running",toolCount:205}],1);
    for(let i=1;i<=205;i++)state=observeSubagentEvent(state,event(i),i);
    expect(state.children[0].toolCount).toBe(205);
  });
  it("limits both observation lanes together to 200 entries per child",()=>{
    let state = observeSubagentRoster(emptySubagentProgress("scope","live"),[{id:"child",goal:"Task",status:"running",toolCount:0}],1);
    state.children[0].nativeJournal=Array.from({length:200},(_,i)=>({id:String(i),kind:"tool_start",toolName:"terminal"}));
    state=observeSubagentEvent(state,event(1),2);
    expect(state.children[0].journal.length+state.children[0].nativeJournal.length).toBeLessThanOrEqual(200);
  });
  it("retains exact terminal outcome after disappearance and refuses late revival", () => {
    let state = observeSubagentRoster(emptySubagentProgress("scope", "live"), [{ id: "child", goal: "Task", status: "running", toolCount: 0 }], 1);
    state = observeSubagentEvent(state, event(1, "subagent.complete", { status: "completed", summary: "PRIVATE" }), 2);
    state = observeSubagentRoster(state, [], 3);
    expect(state.children[0]).toMatchObject({ status: "completed", terminalConfirmed: true, present: false, receivedAt: 2 });
    state = observeSubagentEvent(state, event(2), 4);
    expect(state.children[0].journal).toEqual([]);
    expect(state.children[0].receivedAt).toBe(2);
  });
  it("accepts starts before roster and labels resumed sequence gaps partial", () => {
    let state = observeSubagentEvent(emptySubagentProgress("scope", "live"), event(1, "subagent.start", { goal: "Owned goal", status: "running" }), 10);
    expect(state.children[0]).toMatchObject({ goal: "Owned goal", receivedAt: 10, partial: false });
    state = observeSubagentEvent(state, event(4), 20);
    expect(state.children[0].partial).toBe(true);
    expect(state.children[0].journal).toHaveLength(1);
    const end = observeSubagentEvent(state, event(5, "subagent.complete", { status: "done", summary: "success" }), 30);
    expect(end.children[0]).toMatchObject({ status: "unknown", terminalConfirmed: true });
  });
});
