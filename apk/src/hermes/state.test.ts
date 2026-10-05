import { describe, expect, it } from "vitest";
import { emptyRunState, type HermesEvent } from "./client";
import { applyHermesEvent, stateFromSession } from "./state";

function event(seq: number, type: string, payload: Record<string, unknown> = {}, session_id = "live-A"): HermesEvent {
  return { seq, frame: { method: "event", params: { type, payload, session_id } } };
}

describe("published Hermes reasoning and live progress", () => {
  it("keeps interleaved reasoning and answer fragments in one message and replaces both final values", () => {
    let state = applyHermesEvent(emptyRunState(), event(1, "message.start"), "live-A");
    state = applyHermesEvent(state, event(2, "reasoning.delta", { text: "Read " }), "live-A");
    state = applyHermesEvent(state, event(3, "message.delta", { text: "The " }), "live-A");
    state = applyHermesEvent(state, event(4, "reasoning.delta", { text: "the files." }), "live-A");
    state = applyHermesEvent(state, event(5, "message.delta", { text: "result." }), "live-A");
    expect(state.messages).toHaveLength(1);
    expect(state.messages[0]).toMatchObject({ content: "The result.", reasoning: "Read the files." });
    expect(state.messages[0].id).toBe(state.streamId);
    state = applyHermesEvent(state, event(6, "message.complete", {
      text: "Final result.", reasoning: "Read the files.", status: "complete",
    }), "live-A");
    expect(state.messages).toHaveLength(1);
    expect(state.messages[0]).toMatchObject({ content: "Final result.", reasoning: "Read the files." });
    expect(state.busy).toBe(false);
    expect(state.streamId).toBeNull();
    expect(state.progressText).toBe("");
  });

  it("attaches reasoning that starts after answer streaming to the same active message", () => {
    let state = applyHermesEvent(emptyRunState(), event(1, "message.delta", { text: "Answer" }), "live-A");
    const bodyId = state.streamId;
    state = applyHermesEvent(state, event(2, "reasoning.delta", { text: "Published explanation" }), "live-A");
    expect(state.streamId).toBe(bodyId);
    expect(state.messages).toHaveLength(1);
    expect(state.messages[0]).toMatchObject({ content: "Answer", reasoning: "Published explanation" });
  });

  it("treats reasoning.available as a complete block instead of duplicating streamed fragments", () => {
    let state = applyHermesEvent(emptyRunState(), event(1, "reasoning.delta", { text: "Partial" }), "live-A");
    state = applyHermesEvent(state, event(2, "reasoning.available", { text: "Complete block" }), "live-A");
    state = applyHermesEvent(state, event(3, "reasoning.available", { text: "Complete block" }), "live-A");
    state = applyHermesEvent(state, event(4, "message.complete", { text: "Answer", reasoning: "Complete block" }), "live-A");
    expect(state.messages).toHaveLength(1);
    expect(state.messages[0]).toMatchObject({ content: "Answer", reasoning: "Complete block" });
  });

  it("retains a published reasoning block when completion contains no reasoning field", () => {
    let state = applyHermesEvent(emptyRunState(), event(1, "reasoning.delta", { text: "A real block" }), "live-A");
    state = applyHermesEvent(state, event(2, "message.complete", { text: "Answer" }), "live-A");
    expect(state.messages[0]).toMatchObject({ content: "Answer", reasoning: "A real block" });
  });

  it("preserves a reasoning-only completion without manufacturing an answer body", () => {
    const state = applyHermesEvent(emptyRunState(), event(1, "message.complete", { text: "", reasoning: "Published block" }), "live-A");
    expect(state.messages).toHaveLength(1);
    expect(state.messages[0]).toMatchObject({ content: "", reasoning: "Published block" });
  });

  it("keeps legacy thinking text only in the latest transient status", () => {
    let state = applyHermesEvent(emptyRunState(), event(1, "message.start"), "live-A");
    state = applyHermesEvent(state, event(2, "thinking.delta", { text: "Waiting for provider" }), "live-A");
    state = applyHermesEvent(state, event(3, "thinking.delta", { text: "Retrying provider" }), "live-A");
    expect(state.progressText).toBe("Retrying provider");
    expect(state.messages).toEqual([]);
    expect(state.activities).toEqual([]);
    expect(state.streamId).toBeNull();
    expect(state.busy).toBe(true);
    state = applyHermesEvent(state, event(4, "message.start"), "live-A");
    expect(state.progressText).toBe("");
  });

  it("clears an obsolete provider wait as soon as published output resumes", () => {
    let state = applyHermesEvent(emptyRunState(), event(1, "thinking.delta", { text: "Waiting" }), "live-A");
    state = applyHermesEvent(state, event(2, "reasoning.delta", { text: "Actual reasoning" }), "live-A");
    expect(state.progressText).toBe("");
    state = applyHermesEvent(state, event(3, "thinking.delta", { text: "Waiting again" }), "live-A");
    state = applyHermesEvent(state, event(4, "message.delta", { text: "Actual answer" }), "live-A");
    expect(state.progressText).toBe("");
  });

  it("keeps interim commentary busy and starts the next reasoning segment separately", () => {
    let state = applyHermesEvent(emptyRunState(), event(1, "reasoning.delta", { text: "Before the tool" }), "live-A");
    state = applyHermesEvent(state, event(2, "message.interim", { text: "Checking the files" }), "live-A");
    expect(state.busy).toBe(true);
    expect(state.streamId).toBeNull();
    state = applyHermesEvent(state, event(3, "reasoning.delta", { text: "After the tool" }), "live-A");
    state = applyHermesEvent(state, event(4, "message.complete", { text: "Done", reasoning: "After the tool" }), "live-A");
    expect(state.messages.map(row => [row.content, row.reasoning])).toEqual([
      ["Checking the files", "Before the tool"], ["Done", "After the tool"],
    ]);
    expect(state.busy).toBe(false);
  });

  it("ignores another session's reasoning, progress and final detail", () => {
    const state = emptyRunState();
    for (const type of ["reasoning.delta", "reasoning.available", "thinking.delta", "message.complete", "status.update", "tool.generating"]) {
      expect(applyHermesEvent(state, event(1, type, { text: "Other", reasoning: "Other", name: "Other" }, "live-B"), "live-A")).toBe(state);
    }
  });

  it("shows tool drafting and status text without adding either to the published reasoning", () => {
    let state = applyHermesEvent(emptyRunState(), event(1, "tool.generating", { name: "read_file" }), "live-A");
    expect(state.progressText).toContain("read_file");
    state = applyHermesEvent(state, event(2, "status.update", { kind: "process", text: "Running the check" }), "live-A");
    expect(state.progressText).toBe("Running the check");
    expect(state.messages).toEqual([]);
    expect(state.activities[0].text).toBe("Running the check");
    state = applyHermesEvent(state, event(3, "error", { message: "Provider error" }), "live-A");
    expect(state.progressText).toBe("");
  });

  it("does not invent reasoning for a plain answer or decode unexpected reasoning payloads", () => {
    let state = emptyRunState();
    expect(applyHermesEvent(state, event(1, "reasoning.delta", { text: { signature: "opaque" } }), "live-A")).toBe(state);
    expect(applyHermesEvent(state, event(2, "reasoning.available", { text: "" }), "live-A")).toBe(state);
    state = applyHermesEvent(state, event(3, "message.delta", { text: "Plain " }), "live-A");
    state = applyHermesEvent(state, event(4, "message.complete", { text: "Plain answer", reasoning: { encrypted_content: "opaque" } }), "live-A");
    expect(state.messages).toHaveLength(1);
    expect(state.messages[0].content).toBe("Plain answer");
    expect(state.messages[0]).not.toHaveProperty("reasoning");
  });
});

describe("restoring published reasoning from a Hermes transcript", () => {
  it("restores the assistant's published reasoning, including a reasoning-only row", () => {
    const state = stateFromSession({ session_id: "live-A", messages: [
      { role: "user", text: "Task", row_id: 1 },
      { role: "assistant", text: "Answer", reasoning: "Published explanation", row_id: 2 },
      { role: "assistant", text: "", reasoning: "Published reasoning-only block", row_id: 3 },
    ] });
    expect(state.messages.map(row => [row.content, row.reasoning])).toEqual([
      ["Task", undefined], ["Answer", "Published explanation"], ["", "Published reasoning-only block"],
    ]);
    expect(state.messages.map(row => row.id)).toEqual(["stored-1", "stored-2", "stored-3"]);
    expect(state.progressText).toBe("");
    expect(state.busy).toBe(false);
  });

  it("does not render signature, encrypted or untyped sidecar fields as reasoning", () => {
    const snapshot = { session_id: "live-A", messages: [
      { role: "assistant", text: "Plain answer", reasoning_details: [{ type: "encrypted", data: "opaque" }], codex_reasoning_items: [{ signature: "opaque" }] },
      { role: "user", text: "User text", reasoning: "Not an assistant detail" },
    ] };
    const state = stateFromSession(snapshot);
    expect(state.messages.map(row => row.content)).toEqual(["Plain answer", "User text"]);
    expect(state.messages.every(row => row.reasoning === undefined)).toBe(true);
  });
});

describe("recovering actions and independent child lifecycles", () => {
  it("restores completed tool results apart from answer bubbles and updates a replayed result once", () => {
    let state = stateFromSession({ session_id: "live-A", running: true, messages: [
      { role: "user", text: "Inspect the project", row_id: 1 },
      { role: "tool", name: "read_file", tool_call_id: "tool-A", text: "File contents", row_id: 2 },
      { role: "tool", name: "terminal", content: { exit_code: 0 }, row_id: 3 },
      { role: "assistant", text: "I will continue", row_id: 4 },
    ] });
    expect(state.messages.map(row => row.content)).toEqual(["Inspect the project", "I will continue"]);
    expect(state.activities).toHaveLength(2);
    expect(state.activities[0]).toMatchObject({ id: "tool-A", text: "read_file", details: "File contents", complete: true });
    expect(state.activities[1].details).toContain('"exit_code": 0');
    expect(state.busy).toBe(true);
    state = applyHermesEvent(state, event(1, "tool.complete", { tool_id: "tool-A", name: "read_file", result_text: "Final result" }), "live-A");
    expect(state.activities).toHaveLength(2);
    expect(state.activities[0].details).toBe("Final result");
  });

  it("does not make a completed transcript active because its answer promises more work", () => {
    const state = stateFromSession({ session_id: "live-A", running: false, messages: [
      { role: "tool", name: "delegate_task", tool_call_id: "delegation", text: "Children dispatched" },
      { role: "assistant", text: "Analysis is continuing. I will report later." },
    ] });
    expect(state.busy).toBe(false);
    expect(state.activities.every(row => row.complete)).toBe(true);
  });

  it("keeps one child activity through spawn, tools and completion without changing the main turn", () => {
    let state = applyHermesEvent(emptyRunState(), event(1, "message.start"), "live-A");
    state = applyHermesEvent(state, event(2, "subagent.spawn_requested", { subagent_id: "child-A", goal: "Review code", status: "queued" }), "live-A");
    state = applyHermesEvent(state, event(3, "subagent.start", { subagent_id: "child-A", goal: "Review code", status: "running" }), "live-A");
    state = applyHermesEvent(state, event(4, "subagent.tool", { subagent_id: "child-A", goal: "Review code", tool_preview: "Reading a file" }), "live-A");
    expect(state.activities).toHaveLength(1);
    expect(state.activities[0]).toMatchObject({ subagentId: "child-A", status: "running", complete: false });
    expect(state.activities[0]).not.toHaveProperty("details");
    state = applyHermesEvent(state, event(5, "message.complete", { text: "Delegated analysis continues", status: "complete" }), "live-A");
    expect(state.busy).toBe(false);
    expect(state.activities[0].status).toBe("running");
    state = applyHermesEvent(state, event(6, "subagent.complete", { subagent_id: "child-A", goal: "Review code", summary: "Review finished", status: "completed" }), "live-A");
    expect(state.activities).toHaveLength(1);
    expect(state.activities[0]).toMatchObject({ text: "Review code", status: "completed", complete: true });
    expect(state.busy).toBe(false);
    expect(state.messages.map(row => row.content)).toEqual(["Delegated analysis continues"]);
  });

  it("does not revive a completed child when late progress arrives", () => {
    let state = applyHermesEvent(emptyRunState(), event(1, "subagent.complete", { subagent_id: "child", goal: "Task", status: "completed" }), "live-A");
    const settled = state;
    state = applyHermesEvent(state, event(2, "subagent.progress", { subagent_id: "child", goal: "Task", status: "running", text: "Late tool callback" }), "live-A");
    expect(state).toBe(settled);
  });

  it("keeps failed and interrupted child outcomes separate from the still-running parent", () => {
    for (const [native, expected] of [["failed", "failed"], ["error", "failed"], ["timeout", "failed"], ["interrupted", "cancelled"]]) {
      const parent = { ...emptyRunState(), busy: true };
      const state = applyHermesEvent(parent, event(1, "subagent.complete", { subagent_id: "child", goal: "Task", status: native }), "live-A");
      expect(state.busy).toBe(true);
      expect(state.activities[0]).toMatchObject({ status: expected, complete: true });
    }
  });

  it("retains an unknown completion outcome without guessing success from its summary", () => {
    const state = applyHermesEvent(emptyRunState(), event(1, "subagent.complete", { subagent_id: "child", goal: "Task", summary: "Still working on it" }), "live-A");
    expect(state.activities[0]).toMatchObject({ status: "unknown", complete: true });
    expect(state.busy).toBe(false);
  });

  it("does not merge distinct children or another chat's activity into the current answer", () => {
    let state = applyHermesEvent(emptyRunState(), event(1, "subagent.start", { subagent_id: "child-A", goal: "Task A" }), "live-A");
    state = applyHermesEvent(state, event(2, "subagent.thinking", { subagent_id: "child-A", goal: "Task A", text: "Published child detail" }), "live-A");
    state = applyHermesEvent(state, event(3, "subagent.start", { subagent_id: "child-B", goal: "Task B" }), "live-A");
    expect(state.activities.map(row => row.subagentId)).toEqual(["child-A", "child-B"]);
    expect(state.messages).toEqual([]);
    expect(state.busy).toBe(false);
    expect(applyHermesEvent(state, event(4, "subagent.complete", { subagent_id: "child-A", goal: "Other task", status: "completed" }, "live-B"), "live-A")).toBe(state);
  });
});
