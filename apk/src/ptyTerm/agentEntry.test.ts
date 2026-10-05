import { describe, expect, it } from "vitest";
import { agentEntryMode, type AgentLaunchAttempt } from "./agentEntry";

const shell = { sessionId: "one", alive: true, connected: true, agentKind: "shell", agentRunning: false, attempt: null };
const attempt: AgentLaunchAttempt = { sessionId: "one", name: "Kimi Code", startedAt: 100 };

describe("first agent launch feedback", () => {
  it("identifies the command line before any launch", () => {
    expect(agentEntryMode(shell)).toBe("shell");
  });
  it("a sent command starts waiting without claiming success", () => {
    expect(agentEntryMode({ ...shell, attempt })).toBe("starting");
    expect(agentEntryMode({ ...shell, attempt: { ...attempt, expired: true } })).toBe("unconfirmed");
  });
  it("only the observed process confirms the agent", () => {
    expect(agentEntryMode({ ...shell, agentKind: "kimi", agentRunning: true, attempt })).toBe("agent");
  });
  it("an old launch cannot follow the user to another terminal", () => {
    expect(agentEntryMode({ ...shell, sessionId: "two", attempt })).toBe("shell");
  });
  it("loading, disconnection and a closed terminal never become launch failures", () => {
    const expired = { ...attempt, expired: true };
    expect(agentEntryMode({ ...shell, agentKind: undefined, attempt: expired })).toBe("unknown");
    expect(agentEntryMode({ ...shell, connected: false, attempt: expired })).toBe("unknown");
    expect(agentEntryMode({ ...shell, alive: false, attempt: expired })).toBe("unknown");
  });
  it("an unidentified program is not called a command line", () => {
    expect(agentEntryMode({ ...shell, agentKind: "other" })).toBe("unknown");
  });
});
