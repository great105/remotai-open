import { describe, expect, it } from "vitest";
import {
  LatestOnlyGate,
  advanceAppliedOffset,
  effectiveHistoryOwner,
  foregroundKey,
  normalizeScrollOverride,
  scrollClassifierKey,
  shouldResetScrollClassifier,
  afterSyncMarker,
  writerEpochKey,
} from "./sessionRuntime";

describe("LatestOnlyGate", () => {
  it("rejects an older poll that resolves after a newer one", () => {
    const gate = new LatestOnlyGate("pty-a");
    const slow = gate.begin();
    const fresh = gate.begin();
    expect(gate.accept(slow)).toBe(false);
    expect(gate.accept(fresh)).toBe(true);
  });

  it("rejects every callback after route invalidation", () => {
    const gate = new LatestOnlyGate("pty-a");
    const pending = gate.begin();
    gate.invalidate("pty-b");
    expect(gate.accept(pending)).toBe(false);
  });
});

describe("writer epoch", () => {
  it("changes across a full reset even when the server resume epoch stays the same", () => {
    expect(writerEpochKey("same-server-epoch", 1))
      .not.toBe(writerEpochKey("same-server-epoch", 2));
  });

  it("does not let an old callback raise the applied offset in a new scale", () => {
    const old = { generation: 1, epoch: "old" };
    const fresh = { generation: 1, epoch: "fresh" };
    expect(advanceAppliedOffset(4, 900, old, fresh)).toBe(4);
    expect(advanceAppliedOffset(4, 9, fresh, fresh)).toBe(9);
  });
});

describe("sync marker runtime boundary", () => {
  const active = {
    pageAccum: -7,
    queuedProbeLines: -3,
  } as const;

  it("drops accumulation, queued probe work and live navigation evidence on reset", () => {
    expect(afterSyncMarker("reset", active)).toEqual({
      pageAccum: 0,
      queuedProbeLines: 0,
      resetEvidence: true,
    });
  });

  it("cancels queued probe work but preserves same-stream state on resumed", () => {
    expect(afterSyncMarker("resumed", active)).toEqual({
      ...active,
      queuedProbeLines: 0,
      resetEvidence: false,
    });
  });
});

describe("manual scroll override", () => {
  it("forces terminal output or agent history and leaves auto unchanged", () => {
    expect(effectiveHistoryOwner("application", "auto")).toBe("application");
    expect(effectiveHistoryOwner("application", "terminal")).toBe("terminal");
    expect(effectiveHistoryOwner("terminal", "agent")).toBe("application");
  });

  it("foreground identity changes with process generation, not background output", () => {
    expect(foregroundKey({ fgPid: 42, agentKind: "claude" }))
      .toBe(foregroundKey({ fgPid: 42, agentKind: "claude" }));
    expect(foregroundKey({ fgPid: 42, agentKind: "claude" }))
      .not.toBe(foregroundKey({ fgPid: 43, agentKind: "claude" }));
    expect(foregroundKey({ fgPid: 43, agentKind: "shell" })).toBe("shell:43");
    expect(foregroundKey({ fgPid: 43, agentKind: "" })).toBe("shell:43");
    expect(foregroundKey({ fgPid: 42, agentKind: "claude" }))
      .not.toBe(foregroundKey({ fgPid: 43, agentKind: "" }));
    expect(foregroundKey({ fgPid: 0, agentKind: "shell" })).toBe("");
  });

  it("accepts only the three scroll modes declared by the server contract", () => {
    expect(normalizeScrollOverride("agent")).toBe("agent");
    expect(normalizeScrollOverride("terminal")).toBe("terminal");
    expect(normalizeScrollOverride("auto")).toBe("auto");
    expect(normalizeScrollOverride("unknown")).toBe("auto");
    expect(normalizeScrollOverride()).toBe("auto");
  });

  it("classifies local generations and remote observations independently of the saved mode", () => {
    expect(scrollClassifierKey({ fgPid: 41, agentKind: "claude" })).toBe("claude:41");
    expect(scrollClassifierKey({ fgPid: 0, agentKind: "kimi", remote: true }))
      .toBe("remote-observed:kimi:0");
  });

  it("resets classifier state only between two confirmed generations", () => {
    expect(shouldResetScrollClassifier("", "claude:41")).toBe(false);
    expect(shouldResetScrollClassifier("claude:41", "")).toBe(false);
    expect(shouldResetScrollClassifier("claude:41", "claude:41")).toBe(false);
    expect(shouldResetScrollClassifier("claude:41", "shell:7")).toBe(true);
  });
});
